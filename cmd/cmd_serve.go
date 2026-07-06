package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/auth"
	"github.com/dcadolph/yardmaster/internal/credential"
	"github.com/dcadolph/yardmaster/internal/dispatch"
	"github.com/dcadolph/yardmaster/internal/live"
	"github.com/dcadolph/yardmaster/internal/logutil"
	"github.com/dcadolph/yardmaster/internal/pgstore"
	"github.com/dcadolph/yardmaster/internal/project"
	"github.com/dcadolph/yardmaster/internal/roundhouse"
	"github.com/dcadolph/yardmaster/internal/run"
	"github.com/dcadolph/yardmaster/internal/schedule"
	"github.com/dcadolph/yardmaster/internal/server"
	"github.com/dcadolph/yardmaster/internal/sqlitestore"
)

const (
	// defaultServeAddr is the address the server listens on when --addr is not set.
	defaultServeAddr = ":8080"
	// defaultDBPath is the SQLite database file used when --db is not set.
	defaultDBPath = "yardmaster.db"
	// defaultScheduleInterval is how often the scheduler checks for due schedules.
	defaultScheduleInterval = 15 * time.Second
	// shutdownTimeout bounds how long graceful HTTP shutdown waits for in-flight requests.
	shutdownTimeout = 15 * time.Second
	// readHeaderTimeout bounds how long the server waits to read request headers.
	readHeaderTimeout = 10 * time.Second
)

// serveAddr holds the value of the --addr flag.
var serveAddr string

// serveDB holds the value of the --db flag.
var serveDB string

// scheduleInterval holds the value of the --schedule-interval flag.
var scheduleInterval time.Duration

// serveCmd runs the Yardmaster HTTP server (the dispatcher).
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the Yardmaster server.",
	RunE:  runServe,
}

// init registers serve command flags.
func init() {
	serveCmd.Flags().StringVar(&serveAddr, "addr", defaultServeAddr, "Address the server listens on.")
	serveCmd.Flags().StringVar(&serveDB, "db", defaultDBPath,
		"SQLite file path, or a postgres:// DSN for the PostgreSQL backend.")
	serveCmd.Flags().DurationVar(&scheduleInterval, "schedule-interval", defaultScheduleInterval,
		"How often the scheduler checks for due schedules.")
}

// storeBundle is the store set both database backends expose.
type storeBundle interface {
	// Runs returns the run store.
	Runs() run.Store
	// Schedules returns the schedule store.
	Schedules() schedule.Store
	// Tokens returns the API token store.
	Tokens() auth.Store
	// Credentials returns the execution secret store.
	Credentials() credential.Store
	// Projects returns the git project store.
	Projects() project.Store
	// Close closes the underlying database.
	Close() error
}

// openBundle opens the stores for the --db value: a postgres:// or postgresql:// DSN selects the
// PostgreSQL backend, anything else is a SQLite file path.
func openBundle(db string) (storeBundle, error) {
	if strings.HasPrefix(db, "postgres://") || strings.HasPrefix(db, "postgresql://") {
		return pgstore.Open(db)
	}
	return sqlitestore.Open(db)
}

// projectCacheDir returns where project checkouts live: the user cache directory when available,
// the system temp directory otherwise.
func projectCacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "yardmaster", "projects")
}

// runServe builds the server dependencies and serves until interrupted.
func runServe(cmd *cobra.Command, _ []string) error {
	log, err := logutil.New()
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = log.Sync() }()

	bundle, err := openBundle(serveDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = bundle.Close() }()
	store, schedules := bundle.Runs(), bundle.Schedules()

	sealer := credential.NewSealer(os.Getenv("YARDMASTER_ENCRYPTION_KEY"))
	if !sealer.Enabled() {
		log.Warn("credentials disabled: set YARDMASTER_ENCRYPTION_KEY to enable them")
	}

	hub := live.NewHub()
	runner := roundhouse.NewAnsibleRunner()
	syncer, err := project.NewSyncer(projectCacheDir())
	if err != nil {
		return fmt.Errorf("project cache: %w", err)
	}
	disp := dispatch.New(store, runner, log, dispatch.WithPublisher(hub),
		dispatch.WithCredentials(bundle.Credentials(), sealer),
		dispatch.WithProjects(bundle.Projects(), syncer))
	defer disp.Close()

	scheduler := schedule.NewScheduler(schedules, disp, log,
		schedule.WithInterval(scheduleInterval))
	scheduler.Start()
	defer scheduler.Close()

	httpServer := &http.Server{
		Addr: serveAddr,
		Handler: server.New(store, disp, log, server.WithStreamer(hub),
			server.WithCanceler(disp), server.WithRetrier(disp),
			server.WithSchedules(schedules), server.WithTokens(bundle.Tokens()),
			server.WithCredentials(bundle.Credentials(), sealer),
			server.WithProjects(bundle.Projects())).Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("yardmaster serving", zap.String("addr", serveAddr))
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
		log.Info("shutdown signal received: draining")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return <-errCh
	}
}
