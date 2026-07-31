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

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/demo"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/live"
	"github.com/kordloom/switchtender/internal/logutil"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/server"
)

// demoAddr holds the value of the demo --addr flag.
var demoAddr string

// demoDB holds the value of the demo --db flag.
var demoDB string

// demoNoSeed holds the value of the demo --no-seed flag.
var demoNoSeed bool

// demoSeedOnly holds the value of the demo --seed-only flag.
var demoSeedOnly bool

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
}

// runDemo seeds a database and serves it read-only until interrupted.
func runDemo(cmd *cobra.Command, _ []string) error {
	log, err := logutil.New()
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = log.Sync() }()

	db := demoDB
	if db == "" {
		f, err := os.CreateTemp("", "switchtender-demo-*.db")
		if err != nil {
			return fmt.Errorf("demo database: %w", err)
		}
		db = f.Name()
		_ = f.Close()
	}

	bundle, err := openBundle(db)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = bundle.Close() }()
	store := bundle.Runs()

	hub := live.NewHub()
	disp := dispatch.New(store, roundhouse.NewAnsibleRunner(), log, dispatch.WithPublisher(hub))
	defer disp.Close()

	if demoNoSeed && demoSeedOnly {
		return errors.New("--no-seed and --seed-only are opposites; pass at most one")
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

	sealer := newSealerFromEnv(log)
	// The demo publishes a signing identity too, so a visitor can fetch the trust document and check
	// an exported bundle end to end rather than take the description on faith.
	var demoProducer *audit.Identity
	if id, err := audit.LoadIdentity(filepath.Dir(demoDB)); err != nil {
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
