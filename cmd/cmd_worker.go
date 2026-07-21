package cmd

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/dcadolph/switchtender/internal/dispatch"
	"github.com/dcadolph/switchtender/internal/extplugin"
	"github.com/dcadolph/switchtender/internal/logutil"
	"github.com/dcadolph/switchtender/internal/project"
	"github.com/dcadolph/switchtender/internal/relay"
	"github.com/dcadolph/switchtender/internal/roundhouse"
	"github.com/dcadolph/switchtender/internal/run"
)

// relayClientTimeout bounds each HTTP call a relay worker makes to the control node. The execution
// path calls are short: claim, heartbeat, save, and the log and event appends, none of them a long
// poll, so a modest timeout keeps a stalled control node from wedging the worker.
const relayClientTimeout = 30 * time.Second

// workerDB holds the value of the worker --db flag.
var workerDB string

// workerServer holds the value of the worker --server flag: the control node base URL a relay
// worker dials instead of opening a local database.
var workerServer string

// workerName holds the value of the worker --name flag.
var workerName string

// workerQueues holds the values of the repeatable worker --queue flag.
var workerQueues []string

// workerAllowContainerEE holds the value of the worker --allow-container-ee flag.
var workerAllowContainerEE bool

// workerDefaultImage holds the value of the worker --default-image flag, the fallback execution
// image used when a run, its template, and its project pin none.
var workerDefaultImage string

// workerRequireImageDigest holds the value of the worker --require-image-digest flag.
var workerRequireImageDigest bool

// workerPluginsDir holds the value of the worker --plugins-dir flag.
var workerPluginsDir string

// workerWorkers holds the value of the worker --workers flag.
var workerWorkers int

// workerCmd runs a SwitchTender worker: a process that leases pending runs from the shared store,
// executes them, and streams results back. Point it and a server at the same database, a
// PostgreSQL DSN for separate machines, and they compete for work.
var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Run a SwitchTender worker that executes runs from the shared store.",
	RunE:  runWorker,
}

// init registers worker command flags.
func init() {
	workerCmd.Flags().StringVar(&workerDB, "db", defaultDBPath,
		"SQLite file path, or a postgres:// DSN for the PostgreSQL backend. Ignored with --server.")
	workerCmd.Flags().StringVar(&workerServer, "server", "",
		"Control node base URL to lease runs from over the mesh relay, for example "+
			"https://switchtender.example.com. When set, the worker needs no database and dials one "+
			"outbound connection. Token from SWITCHTENDER_WORKER_TOKEN.")
	workerCmd.Flags().StringVar(&workerName, "name", "",
		"Worker name stamped on the runs it executes. Defaults to host and pid.")
	workerCmd.Flags().StringArrayVar(&workerQueues, "queue", nil,
		"Queue this worker serves. Repeatable. Without any, it serves the default pool.")
	workerCmd.Flags().BoolVar(&workerAllowContainerEE, "allow-container-ee", false,
		"Allow runs whose project pins a container image to execute inside that image. Needs Docker.")
	workerCmd.Flags().StringVar(&workerDefaultImage, "default-image", "",
		"Fallback execution image for runs that pin none at the run, template, or project level. "+
			"Empty leaves an unpinned run on the host.")
	workerCmd.Flags().BoolVar(&workerRequireImageDigest, "require-image-digest", false,
		"Reject a container run whose image is not pinned to an @sha256: digest.")
	workerCmd.Flags().IntVar(&workerWorkers, "workers", dispatch.DefaultWorkers,
		"Concurrent runs this process executes at once.")
	workerCmd.Flags().StringVar(&workerPluginsDir, "plugins-dir", "",
		"Directory of extension plugin binaries to load at startup. Empty loads none. Also SWITCHTENDER_PLUGINS_DIR.")
	registerContainerFlags(workerCmd)
	registerGalaxyFlag(workerCmd)
}

// runWorker leases and executes runs until interrupted.
func runWorker(cmd *cobra.Command, _ []string) error {
	log, err := logutil.New()
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = log.Sync() }()

	closePlugins, err := extplugin.Load(pluginsDir(workerPluginsDir), log)
	if err != nil {
		return fmt.Errorf("load plugins: %w", err)
	}
	defer closePlugins()

	store, opts, closeStore, err := workerStore(log)
	if err != nil {
		return err
	}
	defer closeStore()

	if workerName != "" {
		opts = append(opts, dispatch.WithOwner(workerName))
	}
	if len(workerQueues) > 0 {
		opts = append(opts, dispatch.WithQueues(workerQueues))
	}
	runner := roundhouse.NewSelectiveRunner(workerAllowContainerEE, containerRuntimeFromFlags(),
		containerPullPolicyFromFlags(), workerRequireImageDigest, containerLimitsFromFlags())
	disp := dispatch.New(store, runner, log, opts...)
	defer disp.Close()

	log.Info("switchtender worker started",
		zap.String("owner", disp.Owner()), zap.String("source", workerSource()))

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	log.Info("shutdown signal received: draining")
	return nil
}

// workerStore selects the run store the worker executes against and the dispatch options that go
// with it. With --server set it dials the control node over the mesh relay, so it needs no database
// and disables the janitor the relay Client cannot serve. Without it, it opens the local bundle and
// wires the credential, project, and inventory stores exactly as a direct worker always has. The
// returned close func releases whatever the store holds.
func workerStore(log *zap.Logger) (run.Store, []dispatch.Option, func(), error) {
	opts := []dispatch.Option{
		dispatch.WithWorkers(workerWorkers),
		dispatch.WithDefaultImage(workerDefaultImage),
	}
	if workerServer != "" {
		token := os.Getenv("SWITCHTENDER_WORKER_TOKEN")
		if token == "" {
			return nil, nil, nil, errors.New("open store: relay worker needs SWITCHTENDER_WORKER_TOKEN")
		}
		client := &http.Client{Timeout: relayClientTimeout}
		store := relay.NewClient(relay.NewHTTPTransport(workerServer, token, client))
		// The relay Client cannot reclaim stale leases; that stays the control node's job, so the
		// janitor would only log ErrUnsupported on every sweep. Turn it off for the relay worker.
		opts = append(opts, dispatch.WithNoJanitor())
		return store, opts, func() {}, nil
	}

	bundle, err := openBundle(workerDB)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open store: %w", err)
	}
	sealer := newSealerFromEnv(log)
	syncer, err := project.NewSyncer(projectCacheDir(), galaxySyncerOpts()...)
	if err != nil {
		_ = bundle.Close()
		return nil, nil, nil, fmt.Errorf("project cache: %w", err)
	}
	opts = append(opts,
		dispatch.WithCredentials(bundle.Credentials(), sealer),
		dispatch.WithProjects(bundle.Projects(), syncer),
		dispatch.WithInventories(bundle.Inventories()),
		dispatch.WithInventorySources(bundle.InventorySources()),
	)
	return bundle.Runs(), opts, func() { _ = bundle.Close() }, nil
}

// workerSource describes where the worker leases runs from, for logging: the control node URL in
// relay mode or the redacted database DSN otherwise. It never includes the worker token.
func workerSource() string {
	if workerServer != "" {
		return workerServer
	}
	return redactDSN(workerDB)
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
