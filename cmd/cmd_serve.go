package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/audit"
	"github.com/dcadolph/yardmaster/internal/auth"
	"github.com/dcadolph/yardmaster/internal/credential"
	"github.com/dcadolph/yardmaster/internal/dispatch"
	"github.com/dcadolph/yardmaster/internal/grant"
	"github.com/dcadolph/yardmaster/internal/inventory"
	"github.com/dcadolph/yardmaster/internal/invsource"
	"github.com/dcadolph/yardmaster/internal/live"
	"github.com/dcadolph/yardmaster/internal/logutil"
	"github.com/dcadolph/yardmaster/internal/pgstore"
	"github.com/dcadolph/yardmaster/internal/project"
	"github.com/dcadolph/yardmaster/internal/retention"
	"github.com/dcadolph/yardmaster/internal/roundhouse"
	"github.com/dcadolph/yardmaster/internal/run"
	"github.com/dcadolph/yardmaster/internal/schedule"
	"github.com/dcadolph/yardmaster/internal/server"
	"github.com/dcadolph/yardmaster/internal/sqlitestore"
	"github.com/dcadolph/yardmaster/internal/team"
	"github.com/dcadolph/yardmaster/internal/template"
	"github.com/dcadolph/yardmaster/internal/trigger"
	"github.com/dcadolph/yardmaster/internal/user"
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

// notifyWebhooks holds the values of the repeatable --notify-webhook flag.
var notifyWebhooks []string

// serveAllowContainerEE holds the value of the --allow-container-ee flag.
var serveAllowContainerEE bool

// serveStrictGrants holds the value of the --strict-grants flag.
var serveStrictGrants bool

// retainRuns holds the value of the --retain-runs flag, a duration like 90d.
var retainRuns string

// retainEvents holds the value of the --retain-events flag, a duration like 30d.
var retainEvents string

// retentionInterval holds the value of the --retention-interval flag.
var retentionInterval time.Duration

// smtpAddr holds the value of the --smtp-addr flag, the SMTP server host:port.
var smtpAddr string

// smtpFrom holds the value of the --smtp-from flag, the sender address.
var smtpFrom string

// smtpTo holds the values of the repeatable --smtp-to flag, the recipient addresses.
var smtpTo []string

// smtpUsername holds the value of the --smtp-username flag; the password comes from the
// YARDMASTER_SMTP_PASSWORD environment variable.
var smtpUsername string

// notifyOn holds the value of the --notify-on flag: failure or finish.
var notifyOn string

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
	serveCmd.Flags().StringArrayVar(&notifyWebhooks, "notify-webhook", nil,
		"URL that receives a JSON notification when a run finishes. Repeatable.")
	serveCmd.Flags().BoolVar(&serveAllowContainerEE, "allow-container-ee", false,
		"Allow runs whose project pins a container image to execute inside that image. Needs Docker.")
	serveCmd.Flags().BoolVar(&serveStrictGrants, "strict-grants", false,
		"Deny non-admins access to an object that has no grants, instead of deferring to the role.")
	serveCmd.Flags().StringVar(&retainRuns, "retain-runs", "",
		"Delete terminal runs older than this, for example 90d. Empty keeps them forever.")
	serveCmd.Flags().StringVar(&retainEvents, "retain-events", "",
		"Drop run events and logs older than this, for example 30d. Empty keeps them forever.")
	serveCmd.Flags().DurationVar(&retentionInterval, "retention-interval", retention.DefaultInterval,
		"How often the retention sweeper runs.")
	serveCmd.Flags().StringVar(&smtpAddr, "smtp-addr", "",
		"SMTP server host:port for run notification emails. Empty disables email.")
	serveCmd.Flags().StringVar(&smtpFrom, "smtp-from", "", "Sender address for notification emails.")
	serveCmd.Flags().StringArrayVar(&smtpTo, "smtp-to", nil,
		"Recipient address for notification emails. Repeatable.")
	serveCmd.Flags().StringVar(&smtpUsername, "smtp-username", "",
		"SMTP username. The password comes from YARDMASTER_SMTP_PASSWORD.")
	serveCmd.Flags().StringVar(&notifyOn, "notify-on", "failure",
		"When to email: failure for failed runs only, or finish for every terminal run.")
}

// buildEmailer constructs the SMTP notifier from the flags, or returns nil when email is not
// configured. It reports whether notifications should fire only on failure.
func buildEmailer() (dispatch.Emailer, bool) {
	if smtpAddr == "" || smtpFrom == "" || len(smtpTo) == 0 {
		return nil, true
	}
	var auth smtp.Auth
	if smtpUsername != "" {
		host, _, _ := net.SplitHostPort(smtpAddr)
		auth = smtp.PlainAuth("", smtpUsername, os.Getenv("YARDMASTER_SMTP_PASSWORD"), host)
	}
	return dispatch.NewSMTPEmailer(smtpAddr, smtpFrom, smtpTo, auth), notifyOn != "finish"
}

// parseRetention converts a retention flag into a duration. An empty value means no window. A
// trailing d counts whole days; otherwise the Go duration syntax applies.
func parseRetention(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("invalid retention %q: %w", s, err)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid retention %q: %w", s, err)
	}
	return d, nil
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
	// Templates returns the job template store.
	Templates() template.Store
	// Users returns the account store.
	Users() user.Store
	// Inventories returns the stored inventory store.
	Inventories() inventory.Store
	// Audits returns the audit trail store.
	Audits() audit.Store
	// InventorySources returns the dynamic inventory source store.
	InventorySources() invsource.Store
	// Triggers returns the webhook trigger store.
	Triggers() trigger.Store
	// Teams returns the team store.
	Teams() team.Store
	// Grants returns the per-object access grant store.
	Grants() grant.Store
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
	runner := roundhouse.NewSelectiveRunner(serveAllowContainerEE)
	syncer, err := project.NewSyncer(projectCacheDir())
	if err != nil {
		return fmt.Errorf("project cache: %w", err)
	}
	emailer, onFailureOnly := buildEmailer()
	disp := dispatch.New(store, runner, log, dispatch.WithPublisher(hub),
		dispatch.WithCredentials(bundle.Credentials(), sealer),
		dispatch.WithProjects(bundle.Projects(), syncer),
		dispatch.WithWebhooks(notifyWebhooks),
		dispatch.WithEmail(emailer, onFailureOnly),
		dispatch.WithInventories(bundle.Inventories()),
		dispatch.WithInventorySources(bundle.InventorySources()))
	defer disp.Close()

	scheduler := schedule.NewScheduler(schedules, disp, log,
		schedule.WithInterval(scheduleInterval), schedule.WithTemplates(bundle.Templates()))
	scheduler.Start()
	defer scheduler.Close()

	runsWindow, err := parseRetention(retainRuns)
	if err != nil {
		return err
	}
	eventsWindow, err := parseRetention(retainEvents)
	if err != nil {
		return err
	}
	sweeper := retention.NewSweeper(store, log,
		retention.WithRetainRuns(runsWindow), retention.WithRetainEvents(eventsWindow),
		retention.WithInterval(retentionInterval))
	sweeper.Start()
	defer sweeper.Close()

	httpServer := &http.Server{
		Addr: serveAddr,
		Handler: server.New(store, disp, log, server.WithStreamer(hub),
			server.WithCanceler(disp), server.WithRetrier(disp),
			server.WithSchedules(schedules), server.WithTokens(bundle.Tokens()),
			server.WithCredentials(bundle.Credentials(), sealer),
			server.WithProjects(bundle.Projects()),
			server.WithTemplates(bundle.Templates()),
			server.WithUsers(bundle.Users()),
			server.WithInventories(bundle.Inventories()),
			server.WithAudit(bundle.Audits()),
			server.WithInventorySources(bundle.InventorySources(), disp),
			server.WithTriggers(bundle.Triggers()),
			server.WithTeams(bundle.Teams()),
			server.WithGrants(bundle.Grants(), serveStrictGrants),
			server.WithDocs(docsFS)).Handler(),
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
