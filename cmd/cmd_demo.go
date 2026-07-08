package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/demo"
	"github.com/dcadolph/yardmaster/internal/dispatch"
	"github.com/dcadolph/yardmaster/internal/live"
	"github.com/dcadolph/yardmaster/internal/logutil"
	"github.com/dcadolph/yardmaster/internal/roundhouse"
	"github.com/dcadolph/yardmaster/internal/server"
)

// demoAddr holds the value of the demo --addr flag.
var demoAddr string

// demoDB holds the value of the demo --db flag.
var demoDB string

// demoCmd runs a seeded, read-only Yardmaster instance for evaluation. It fills a fresh database
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
		f, err := os.CreateTemp("", "yardmaster-demo-*.db")
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

	log.Info("demo: seeding sample data, this runs a few playbooks and takes a moment")
	seedDeps := demo.Deps{
		Submitter: disp, Runs: store, Projects: bundle.Projects(),
		Inventories: bundle.Inventories(), Templates: bundle.Templates(),
		Credentials: bundle.Credentials(),
	}
	if err := demo.Seed(cmd.Context(), seedDeps, log); err != nil {
		return fmt.Errorf("seed demo: %w", err)
	}

	sealer := newSealerFromEnv(log)
	httpServer := &http.Server{
		Addr: demoAddr,
		Handler: server.New(store, disp, log, server.WithStreamer(hub),
			server.WithSchedules(bundle.Schedules()),
			server.WithProjects(bundle.Projects()),
			server.WithTemplates(bundle.Templates()),
			server.WithInventories(bundle.Inventories()),
			server.WithCredentials(bundle.Credentials(), sealer),
			server.WithInventorySources(bundle.InventorySources(), disp),
			server.WithTriggers(bundle.Triggers()),
			server.WithTeams(bundle.Teams()),
			server.WithGrants(bundle.Grants(), false),
			server.WithAudit(bundle.Audits()),
			server.WithDocs(docsFS),
			server.WithReadOnly(true)).Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("yardmaster demo serving (read-only)", zap.String("addr", demoAddr))
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
