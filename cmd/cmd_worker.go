package cmd

import (
	"fmt"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/dispatch"
	"github.com/dcadolph/yardmaster/internal/logutil"
	"github.com/dcadolph/yardmaster/internal/roundhouse"
)

// workerDB holds the value of the worker --db flag.
var workerDB string

// workerName holds the value of the worker --name flag.
var workerName string

// workerCmd runs a Yardmaster worker: a process that leases pending runs from the shared store,
// executes them, and streams results back. Point it and a server at the same database, a
// PostgreSQL DSN for separate machines, and they compete for work.
var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Run a Yardmaster worker that executes runs from the shared store.",
	RunE:  runWorker,
}

// init registers worker command flags.
func init() {
	workerCmd.Flags().StringVar(&workerDB, "db", defaultDBPath,
		"SQLite file path, or a postgres:// DSN for the PostgreSQL backend.")
	workerCmd.Flags().StringVar(&workerName, "name", "",
		"Worker name stamped on the runs it executes. Defaults to host and pid.")
}

// runWorker leases and executes runs until interrupted.
func runWorker(cmd *cobra.Command, _ []string) error {
	log, err := logutil.New()
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = log.Sync() }()

	bundle, err := openBundle(workerDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = bundle.Close() }()
	store := bundle.Runs()

	opts := []dispatch.Option{}
	if workerName != "" {
		opts = append(opts, dispatch.WithOwner(workerName))
	}
	disp := dispatch.New(store, roundhouse.NewAnsibleRunner(), log, opts...)
	defer disp.Close()

	log.Info("yardmaster worker started",
		zap.String("owner", disp.Owner()), zap.String("db", redactDSN(workerDB)))

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	log.Info("shutdown signal received: draining")
	return nil
}

// redactDSN hides credentials in a database DSN for logging.
func redactDSN(dsn string) string {
	if at := lastAt(dsn); at != -1 {
		if scheme := schemeEnd(dsn); scheme != -1 && scheme < at {
			return dsn[:scheme] + "***" + dsn[at:]
		}
	}
	return dsn
}

// lastAt returns the index of the last @ in s, or -1.
func lastAt(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '@' {
			return i
		}
	}
	return -1
}

// schemeEnd returns the index just past "://" in s, or -1.
func schemeEnd(s string) int {
	for i := 0; i+2 < len(s); i++ {
		if s[i] == ':' && s[i+1] == '/' && s[i+2] == '/' {
			return i + 3
		}
	}
	return -1
}
