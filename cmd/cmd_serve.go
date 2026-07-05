package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/dispatch"
	"github.com/dcadolph/yardmaster/internal/live"
	"github.com/dcadolph/yardmaster/internal/logutil"
	"github.com/dcadolph/yardmaster/internal/roundhouse"
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
	serveCmd.Flags().StringVar(&serveDB, "db", defaultDBPath, "Path to the SQLite database file.")
	serveCmd.Flags().DurationVar(&scheduleInterval, "schedule-interval", defaultScheduleInterval,
		"How often the scheduler checks for due schedules.")
}

// runServe builds the server dependencies and serves until interrupted.
func runServe(cmd *cobra.Command, _ []string) error {
	log, err := logutil.New()
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = log.Sync() }()

	db, err := sqlitestore.Open(serveDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = db.Close() }()
	store := db.Runs()

	hub := live.NewHub()
	runner := roundhouse.NewAnsibleRunner()
	disp := dispatch.New(store, runner, log, dispatch.WithPublisher(hub))
	defer disp.Close()

	if n, err := disp.Reconcile(context.Background()); err != nil {
		log.Warn("reconcile interrupted runs: " + err.Error())
	} else if n > 0 {
		log.Info("reconciled interrupted runs", zap.Int("count", n))
	}

	scheduler := schedule.NewScheduler(db.Schedules(), disp, log,
		schedule.WithInterval(scheduleInterval))
	scheduler.Start()
	defer scheduler.Close()

	httpServer := &http.Server{
		Addr:              serveAddr,
		Handler: server.New(store, disp, log, server.WithStreamer(hub),
			server.WithCanceler(disp), server.WithSchedules(db.Schedules())).Handler(),
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
