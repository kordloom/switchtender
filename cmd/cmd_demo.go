package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/demo"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/live"
	"github.com/kordloom/switchtender/internal/logutil"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/server"
	"github.com/kordloom/switchtender/spanbeat"
)

// demoAddr holds the value of the demo --addr flag.
var demoAddr string

// demoDB holds the value of the demo --db flag.
var demoDB string

// demoNoSeed holds the value of the demo --no-seed flag.
var demoNoSeed bool

// demoSeedOnly holds the value of the demo --seed-only flag.
var demoSeedOnly bool

// demoSpanCadence holds the value of the demo --span-cadence flag, how often a span beat is
// appended to the audit chain. Zero leaves beats off.
var demoSpanCadence time.Duration

// demoCmd runs a seeded, read-only SwitchTender instance for evaluation. It fills a fresh database
// with sample projects, templates, inventories, and real runs, then serves it with every mutating
// request rejected, so it is safe to expose publicly.
var demoCmd = &cobra.Command{
	Use:   "demo",
	Short: "Run a seeded, read-only demo instance.",
	RunE:  runDemo,
}

// init registers demo command flags.
func init() {
	demoCmd.Flags().StringVar(&demoAddr, "addr", defaultServeAddr, "Address the demo listens on.")
	demoCmd.Flags().StringVar(&demoDB, "db", "",
		"Database to seed and serve. Empty uses a fresh temporary SQLite file.")
	demoCmd.Flags().BoolVar(&demoNoSeed, "no-seed", false,
		"Serve the database as it already stands instead of seeding it. Use with a database a "+
			"previous --seed-only run prepared, so a public demo can swap in fresh data without "+
			"a gap in service.")
	demoCmd.Flags().BoolVar(&demoSeedOnly, "seed-only", false,
		"Seed the database and exit without serving, so the result can be swapped in later.")
	demoCmd.Flags().DurationVar(&demoSpanCadence, "span-cadence", 0,
		"Append a span beat to the audit chain this often, for example 60s. Whole seconds only. "+
			"Zero leaves beats off.")
}

// demoPaths returns the database the demo seeds and serves, and the directory its producer identity
// lives in. An empty --db means a fresh temporary SQLite file.
//
// The identity follows the resolved database, the way serve and audit bundle do, rather than the
// raw flag. Reading the flag put it in the working directory whenever --db was empty, since the
// directory of an empty path is the current one. That is where a default install keeps its
// production key, because the default database path is a bare filename, so running the demo from a
// serve directory published the production install identity, public key, and fingerprint on the
// demo's unauthenticated trust document. Run anywhere else it dropped a fresh private key into an
// unrelated directory.
func demoPaths() (db, keyDir string, err error) {
	db = demoDB
	if db == "" {
		f, err := os.CreateTemp("", "switchtender-demo-*.db")
		if err != nil {
			return "", "", fmt.Errorf("demo database: %w", err)
		}
		db = f.Name()
		_ = f.Close()
	}
	return db, filepath.Dir(db), nil
}

// runDemo seeds a database and serves it read-only until interrupted.
func runDemo(cmd *cobra.Command, _ []string) error {
	log, err := logutil.New()
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = log.Sync() }()

	db, keyDir, err := demoPaths()
	if err != nil {
		return err
	}

	bundle, err := openBundle(db)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = bundle.Close() }()
	store := bundle.Runs()

	hub := live.NewHub()
	// A clock parked in the recent past so seeded runs carry a believable history: their records, the
	// audit entries that record their outcomes, and the receipts built from those entries all agree on
	// a time hours ago rather than the single instant the seed ran. The seeder steps it between runs.
	// It governs only the run and outcome timestamps, never the leases the store ages against.
	seedClock := demo.NewSeedClock()
	// The demo serves the policy endpoints, so it enforces them too. Displaying policies that gate
	// nothing teaches the wrong thing about how the product behaves.
	// The stale-lease janitor is off for the demo. It abandons a split or pipeline parent still pending
	// past a cutoff it measures against the real clock, which assumes the parent's created time tracks
	// that clock. The seed clock deliberately parks created times hours in the past, so every fresh
	// parent would read as long abandoned and be interrupted the instant it was submitted. The demo is
	// one process driving every run to completion in hand, with no dead coordinator to reclaim, so the
	// janitor has nothing to do here anyway.
	disp := dispatch.New(store, roundhouse.NewAnsibleRunner(), log, dispatch.WithPublisher(hub),
		dispatch.WithAudits(bundle.Audits()),
		dispatch.WithClock(seedClock.Now),
		dispatch.WithNoJanitor(),
		dispatch.WithPolicies(bundle.Policies()))
	defer disp.Close()

	if demoNoSeed && demoSeedOnly {
		return errors.New("--no-seed and --seed-only are opposites; pass at most one")
	}
	if demoSpanCadence != 0 && (demoSpanCadence < time.Second || demoSpanCadence%time.Second != 0) {
		return fmt.Errorf("--span-cadence must be a whole number of seconds, at least 1s, got %s",
			demoSpanCadence)
	}
	if demoNoSeed {
		log.Info("demo: serving the database as it stands, no seeding")
	} else {
		log.Info("demo: seeding sample data, this runs a few playbooks and takes a moment")
	}
	seedDeps := demo.Deps{
		Submitter: disp, Runs: store, Projects: bundle.Projects(),
		Inventories: bundle.Inventories(), Templates: bundle.Templates(),
		Credentials: bundle.Credentials(),
		Policies:    bundle.Policies(), Users: bundle.Users(),
		InvSources: bundle.InventorySources(),
		Audit:      bundle.Audits(),
		Schedules:  bundle.Schedules(),
		Clock:      seedClock,
	}
	if !demoNoSeed {
		if err := demo.Seed(cmd.Context(), seedDeps, log); err != nil {
			return fmt.Errorf("seed demo: %w", err)
		}
	}
	if demoSeedOnly {
		log.Info("demo: seeded, exiting without serving")
		return nil
	}

	// The demo serves read-only, but beats are server-initiated writes to the audit store rather
	// than API mutations, so the live feed works there too. A --seed-only run returns above and
	// never starts the emitter.
	if demoSpanCadence > 0 {
		beats := spanbeat.NewEmitter(auditBeatStore{store: bundle.Audits()}, demoSpanCadence, log)
		beats.Start()
		defer beats.Close()
		log.Info("span beats enabled", zap.Duration("cadence", demoSpanCadence))
	}

	sealer := newSealerFromEnv(log)
	// The demo publishes a signing identity too, so a visitor can fetch the trust document and check
	// an exported bundle end to end rather than take the description on faith.
	var demoProducer *audit.Identity
	if id, err := audit.LoadIdentity(keyDir); err != nil {
		log.Warn("producer identity unavailable: " + err.Error())
	} else {
		demoProducer = &id
	}
	httpServer := &http.Server{
		Addr: demoAddr,
		Handler: server.New(store, disp, log, server.WithStreamer(hub),
			server.WithSchedules(bundle.Schedules()),
			server.WithProjects(bundle.Projects()),
			server.WithTemplates(bundle.Templates()),
			server.WithInventories(bundle.Inventories()),
			server.WithCredentials(bundle.Credentials(), sealer),
			server.WithInventorySources(bundle.InventorySources(), disp),
			server.WithTriggers(bundle.Triggers(), sealer),
			server.WithTeams(bundle.Teams()),
			// The users page asks for organizations, so without this the demo answered 404 on a
			// page a visitor is invited to open, and showed no organizations at all. Serve wires
			// this; the demo did not, which made a shipped feature invisible in the one place it
			// is being shown off.
			server.WithOrgs(bundle.Orgs()),
			server.WithGrants(bundle.Grants(), false),
			server.WithAudit(bundle.Audits()),
			server.WithProducerIdentity(demoProducer, resolveVersion()),
			server.WithPolicies(bundle.Policies()),
			server.WithUsers(bundle.Users()),
			server.WithApprover(disp),
			server.WithDocs(docsFS),
			server.WithReadOnly(true)).Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("switchtender demo serving (read-only)", zap.String("addr", demoAddr))
		serveErr := httpServer.ListenAndServe()
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		errCh <- serveErr
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return <-errCh
	}
}
