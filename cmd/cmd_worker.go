package cmd

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/extplugin"
	"github.com/kordloom/switchtender/internal/handoff"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/logutil"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// relayClientTimeout bounds each HTTP call a relay worker makes to the control node. The execution
// path calls are short: claim, heartbeat, save, and the log and event appends, none of them a long
// poll, so a modest timeout keeps a stalled control node from wedging the worker.
const relayClientTimeout = 30 * time.Second

// workerPolicyFile names a YAML file holding the approval policies, the same source of truth the
// control node reads with its own --policy-file.
//
// The plan-content gate is enforced by whichever process claims the run, and a worker read the
// policies only from the database. An install that pins its policies to a file leaves that table
// empty, so a worker that won the claim race applied a destroy with no plan, no hold, and no error,
// while the same run was held whenever the control node claimed it instead. Enforcement of the
// product's central control became a coin flip decided by which process happened to be free.
var workerPolicyFile string

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

// workerFactsInterval holds the value of the worker --facts-interval flag.
var workerFactsInterval time.Duration

// workerRetainFacts holds the value of the worker --retain-facts flag.
var workerRetainFacts int

// workerRunTimeout holds the value of the worker --run-timeout flag, the default cap on how long a
// run may execute. Zero disables the cap.
var workerRunTimeout time.Duration

// workerDeliveryKeys holds the values of the repeatable worker --delivery-key flag: the private key
// files a relay worker opens the run secrets its control node seals to the worker's pool with.
var workerDeliveryKeys []string

// workerCmd runs a SwitchTender worker: a process that leases pending runs from the shared store,
// executes them, and streams results back. Point it and a server at the same database, a
// PostgreSQL DSN for separate machines, and they compete for work.
var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Run a SwitchTender worker that executes runs from the shared store.",
	Args:  cobra.NoArgs,
	RunE:  runWorker,
}

// init registers worker command flags.
func init() {
	workerCmd.Flags().StringVar(&workerDB, "db", defaultDBPath,
		"SQLite file path, or a postgres:// DSN for the PostgreSQL backend. "+dbEnvVar+" sets it when "+
			"this flag is absent. With --server the worker opens no database, and the value only "+
			"places the managed Ansible runtime, in ansible/ beside it.")
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
	workerCmd.Flags().DurationVar(&workerFactsInterval, "facts-interval", run.DefaultFactsInterval,
		"Minimum spacing between retained host state snapshots, for example 24h. Zero keeps every "+
			"gather. Applies only to a worker holding its own database with --db: a worker using "+
			"--server reports what it gathered to the control node, which spaces and bounds the "+
			"history with its own setting.")
	workerCmd.Flags().IntVar(&workerRetainFacts, "retain-facts", run.DefaultFactsDepth,
		"Keep only this many host state snapshots for each host. Zero keeps every snapshot forever.")
	workerCmd.Flags().IntVar(&workerWorkers, "workers", dispatch.DefaultWorkers,
		"Concurrent runs this process executes at once.")
	workerCmd.Flags().DurationVar(&workerRunTimeout, "run-timeout", 0,
		"Default cap on how long a run may execute before it is canceled and failed, for example 1h. "+
			"A run may set a shorter timeout. Zero leaves runs uncapped.")
	workerCmd.Flags().StringVar(&workerPolicyFile, "policy-file", "",
		"YAML file holding the approval policies, the same file the control node reads. Set it "+
			"wherever the control node runs with one: on a file-pinned install the policy table is "+
			"empty, so without this a worker enforces nothing and the gate depends on which process "+
			"claims the run.")
	workerCmd.Flags().StringArrayVar(&workerDeliveryKeys, "delivery-key", nil,
		"Private key file a relay worker opens the run secrets its control node seals to this "+
			"worker's pool with, made by switchtender worker key new. Repeatable, so a worker holds "+
			"its pool's old and new key while the pool rotates. Only with --server.")
	workerCmd.Flags().StringVar(&workerPluginsDir, "plugins-dir", "",
		"Directory of extension plugin binaries to load at startup. Empty loads none. Also SWITCHTENDER_PLUGINS_DIR.")
	registerContainerFlags(workerCmd)
	registerRunFilesFlag(workerCmd)
	registerModuleFetchFlags(workerCmd)
	registerGalaxyFlag(workerCmd)
	registerAnsibleFlags(workerCmd)
	registerFederationFlag(workerCmd)
}

// runWorker leases and executes runs until interrupted.
func runWorker(cmd *cobra.Command, _ []string) error {
	workerDB = dbFromEnv(cmd, workerDB)
	log, err := logutil.New()
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = log.Sync() }()
	protectProcess(log)
	if err := applyEgressProxy(log); err != nil {
		return err
	}
	if err := checkWorkers(workerWorkers, workerWorkersHint); err != nil {
		return err
	}
	if err := checkModuleFetchLimits(moduleFetchTimeout, moduleFetchMaxMiB); err != nil {
		return err
	}
	if err := checkModuleKeep(moduleKeepFor, moduleKeepMaxMiB); err != nil {
		return err
	}
	if err := checkContainerChoices(); err != nil {
		return err
	}
	if err := refusePublishedKey(); err != nil {
		return err
	}
	if err := refuseBadAuditKey(); err != nil {
		return err
	}
	if len(workerDeliveryKeys) > 0 && workerServer == "" {
		return fmt.Errorf("%w: --delivery-key opens secrets a control node seals to a relay "+
			"worker, so it needs --server; a worker with --db opens them from the database itself",
			ErrUsage)
	}
	_, runFilesReport, err := prepareRunFiles(log)
	if err != nil {
		return err
	}
	run.SetFactsInterval(workerFactsInterval)
	run.SetFactsDepth(workerRetainFacts)
	// A worker is distributed execution, which is Team. The license sits beside the shared
	// database the worker points at, so the worker and the server read the same answer.
	if lic, lerr := license.Load(license.PathFor(workerDB)); lerr == nil && lic != nil {
		license.Set(lic)
	}
	if err := license.Allow(license.FeatureWorkers); err != nil {
		return err
	}

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

	opts = append(opts, dispatch.WithRunFilesRoot(runFilesReport.Root))
	if workerName != "" {
		opts = append(opts, dispatch.WithOwner(workerName))
	}
	if len(workerQueues) > 0 {
		opts = append(opts, dispatch.WithQueues(workerQueues))
	}
	// The license is read before every claim, not only here at startup. A worker exists to run
	// distributed execution, which is the paid feature, and the process then sits on a signal for
	// as long as the operator leaves it up, so the check it passed the morning it started says
	// nothing about the term a year later. The register emitter has the same shape and reads its
	// license on every tick.
	//
	// A lapse stops it taking new work and nothing else: runs already executing finish and the
	// daemon stays up, because killing an operator's process mid-run is the bricking every other
	// lapse path was fixed to avoid.
	opts = append(opts, dispatch.WithClaimGate(func() error {
		return license.Allow(license.FeatureWorkers)
	}))
	runner := newSelectiveRunnerFromFlags(workerAllowContainerEE, workerRequireImageDigest,
		ansibleLocator(workerDB))
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
		dispatch.WithRunTimeout(workerRunTimeout),
		moduleFetchOption(),
		moduleKeepOption(),
	}
	if workerServer != "" {
		token := os.Getenv("SWITCHTENDER_WORKER_TOKEN")
		if token == "" {
			return nil, nil, nil, errors.New("open store: relay worker needs SWITCHTENDER_WORKER_TOKEN")
		}
		// The delivery keys are read before anything is dialed, so a key file another account can
		// read, or one that is not a key, stops the worker at startup rather than at its first run.
		ring, err := loadDeliveryKeys(workerDeliveryKeys)
		if err != nil {
			return nil, nil, nil, err
		}
		client := &http.Client{Timeout: relayClientTimeout}
		transport := relay.NewHTTPTransport(workerServer, token, client)
		store := relay.NewClient(transport)
		if ring.Len() > 0 {
			log.Info("relay secret delivery enabled", zap.Strings("delivery_keys", ring.IDs()))
		}
		// The relay Client cannot reclaim stale leases; that stays the control node's job, so the
		// janitor would only log ErrUnsupported on every sweep. Turn it off for the relay worker.
		//
		// The plan-content gate runs where the run executes, so a worker reads the approval policies
		// across the relay rather than from a database it has no route to. Without them it would
		// apply a gated terraform change with no plan and no approver whenever it won the claim
		// ahead of the control node.
		//
		// A relay worker has no credential store and no encryption key. A run's secrets arrive sealed
		// to its pool's delivery key with the claim, and the receiver opens them in memory when the run
		// first needs one. It is set even with no key, so a delivery the worker cannot open, or one the
		// control node refused, fails the run with that reason rather than an unexplained missing key.
		opts = append(opts, dispatch.WithNoJanitor(),
			dispatch.WithPolicies(relay.NewPolicyClient(transport)),
			dispatch.WithSecretDelivery(relay.NewReceiver(transport, ring)))
		return store, opts, func() {}, nil
	}

	bundle, err := openBundle(workerDB)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open store: %w", err)
	}
	// The policies come from the file when one is configured, exactly as the control node reads them,
	// so both processes weigh the same rules. A malformed file stops the worker rather than letting it
	// start enforcing nothing, which is the choice serve makes for the same reason.
	policies := bundle.Policies()
	if workerPolicyFile != "" {
		filePolicies, perr := policy.NewFileStore(workerPolicyFile)
		if perr != nil {
			_ = bundle.Close()
			return nil, nil, nil, perr
		}
		policies = filePolicies
	}
	sealer := newSealerFromEnv(log)
	syncer, err := project.NewSyncer(projectCacheDir(), galaxySyncerOpts(ansibleLocator(workerDB))...)
	if err != nil {
		_ = bundle.Close()
		return nil, nil, nil, fmt.Errorf("project cache: %w", err)
	}
	// A worker signs the identity tokens of the runs it executes with the keys every process on
	// this database shares, so a token it mints verifies against the set the server publishes.
	issuer, err := newFederationIssuer(federationIssuer, bundle.FederationKeys(), sealer,
		bundle.Audits())
	if err != nil {
		_ = bundle.Close()
		return nil, nil, nil, err
	}
	opts = append(opts,
		dispatch.WithCredentials(bundle.Credentials(), sealer),
		dispatch.WithCredentialTypes(bundle.CredentialTypes()),
		dispatch.WithFederation(issuer),
		dispatch.WithProjects(bundle.Projects(), syncer),
		dispatch.WithInventories(bundle.Inventories()),
		dispatch.WithInventorySources(bundle.InventorySources()),
		dispatch.WithFactCache(bundle.FactCache()),
		// The chain is written by whichever process finishes the run. Without this a worker executed
		// runs and committed no outcome for any of them: not receiptable, absent from their own
		// dossiers, invisible to the offline verification the product rests on, and silent about it,
		// so a scaled deployment looked healthy while most of its evidence simply did not exist.
		// PostgreSQL serializes the append with an advisory lock, which is the backend a fleet runs
		// on; SQLite is a single-process deployment by design.
		dispatch.WithAudits(bundle.Audits()),
		// The plan-content gate is enforced by whichever process claims the run, so a worker needs
		// the policies as much as the control node does, and from the same place it does.
		dispatch.WithPolicies(policies),
		// A named notification target is told about a run by whichever process starts and finishes
		// it, the same as the chain entry above, or a fleet's runs would reach no target at all.
		// The event is recorded in the shared outbox, so any process on the database delivers it.
		dispatch.WithNotificationOutbox(notificationOutbox(bundle, sealer, log)),
		// A worker on the database reports itself so the dashboard can tell a queue it serves from
		// one nothing serves. A relay worker is reported by the control node it claims from.
		dispatch.WithPresence(bundle.Attention()),
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
	if at := strings.LastIndexByte(dsn, '@'); at != -1 {
		if scheme := strings.Index(dsn, "://"); scheme != -1 && scheme+3 < at {
			return dsn[:scheme+3] + "***" + dsn[at:]
		}
	}
	return dsn
}

// loadDeliveryKeys reads each --delivery-key file into one key ring. A file that is not a delivery
// key, or that another account can read, is refused, and so is the same key named twice.
func loadDeliveryKeys(paths []string) (*handoff.KeyRing, error) {
	keys := make([]*handoff.PrivateKey, 0, len(paths))
	for _, path := range paths {
		k, err := handoff.LoadPrivateKeyFile(path)
		if err != nil {
			return nil, fmt.Errorf("--delivery-key: %w", err)
		}
		keys = append(keys, k)
	}
	ring, err := handoff.NewKeyRing(keys...)
	if err != nil {
		return nil, fmt.Errorf("--delivery-key: %w", err)
	}
	return ring, nil
}
