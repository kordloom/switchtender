package dispatch

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/factcache"
	"github.com/kordloom/switchtender/internal/federation"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	named "github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/runfiles"
)

// Option configures a Dispatcher.
type Option func(*config)

// config holds optional Dispatcher settings before construction.
type config struct {
	// notifyClient dials notification targets. Nil uses the guarded default, which refuses this
	// server itself; a test serving on loopback sets its own.
	notifyClient *http.Client
	// outbox records and delivers the events named notification targets hear, nil to deliver them
	// directly through the router.
	outbox *named.Outbox
	// audits commits each run's outcome to the audit chain, nil when no trail is kept.
	audits audit.Store
	// workers is the worker pool size.
	workers int
	// maxShards caps how many groups a split fans out into.
	maxShards int
	// publisher receives live output for streaming.
	publisher Publisher
	// owner identifies this process on leases.
	owner string
	// claimInterval is how often the claim loop polls when idle.
	claimInterval time.Duration
	// queues names the queues this process serves; empty serves the default pool.
	queues []string
	// credentials resolves stored execution secrets, nil when the feature is off.
	credentials credential.Store
	// credentialTypes resolves operator-defined credential types, nil when none are configured.
	credentialTypes credential.TypeStore
	// federation mints run identity tokens for federated credentials, nil when no issuer is set.
	federation *federation.Issuer
	// runFilesRoot is the directory each run's private credential directory is created under. Empty
	// uses runfiles.DefaultRoot.
	runFilesRoot string
	// sealer decrypts credential secrets.
	sealer *credential.Sealer
	// delivery hands a relay worker the secrets the control node sealed for each run at claim.
	delivery SecretDelivery
	// projects resolves git projects, nil when the feature is off.
	projects project.Store
	// syncer maintains project checkouts.
	syncer *project.Syncer
	// webhooks receive terminal run notifications.
	webhooks []string
	// slackWebhooks receive a Slack-formatted terminal run notification.
	slackWebhooks []string
	// mattermostWebhooks receive the Slack-compatible payload at a Mattermost incoming webhook.
	mattermostWebhooks []string
	// rocketChatWebhooks receive the Slack-compatible payload at a Rocket.Chat incoming webhook.
	rocketChatWebhooks []string
	// discordWebhooks receive a Discord-formatted terminal run notification.
	discordWebhooks []string
	// teamsWebhooks receive a Microsoft Teams Adaptive Card terminal run notification.
	teamsWebhooks []string
	// ntfyURLs receive a terminal run notification published to an ntfy topic.
	ntfyURLs []string
	// ntfyToken is an optional bearer token for a protected ntfy topic.
	ntfyToken string
	// pagerdutyKeys are PagerDuty Events API routing keys that receive an incident when a run fails.
	pagerdutyKeys []string
	// grafanaURLs are Grafana base URLs that receive an annotation when a run finishes.
	grafanaURLs []string
	// grafanaToken is the bearer token for the Grafana annotations API.
	grafanaToken string
	// twilioSID and twilioToken authenticate the Twilio SMS API; twilioFrom is the sender number and
	// twilioTo the recipients that receive an SMS when a run fails.
	twilioSID   string
	twilioToken string
	twilioFrom  string
	twilioTo    []string
	// emailer sends terminal run notifications by email, nil when email is off.
	emailer Emailer
	// emailOnFailureOnly limits email notifications to failed runs.
	emailOnFailureOnly bool
	// router finds the named notification targets attached to what a run came from, nil when none
	// are configured.
	router NotificationRouter
	// inventories resolves stored inventories, nil when the feature is off.
	inventories inventory.Store
	// invSources resolves dynamic inventory sources, nil when the feature is off.
	invSources invsource.Store
	// factCache holds the facts a template's fact cache serves, nil when no executor here can.
	factCache factcache.Store
	// syncSources enables the background scheduled-sync loop for dynamic inventory sources.
	syncSources bool
	// policies gate submitted runs by holding matches for approval, nil when enforcement is off.
	policies policy.Store
	// decisions keeps decision records. Nil uses an in-memory store, which a process that never
	// decides anything, a relay worker, needs no more than.
	decisions decision.Store
	// defaultImage is the fallback execution image used when a run, its template, and its project pin
	// none. Empty leaves an unpinned run on the host.
	defaultImage string
	// imageResolver resolves a run's image tag to the digest its registry serves when the run is
	// submitted. Nil leaves every image bound to its tag.
	imageResolver ImageResolver
	// moduleFetchTimeout bounds how long the gate's module download may run. Zero keeps the default.
	moduleFetchTimeout time.Duration
	// moduleFetchMaxBytes bounds what the gate's module download may write. Zero keeps the default.
	moduleFetchMaxBytes int64
	// moduleKeepFor is how long a module tree the gate downloaded is kept for its run. Zero keeps
	// the default.
	moduleKeepFor time.Duration
	// moduleKeepMaxBytes bounds what the kept module trees occupy. Zero keeps the default.
	moduleKeepMaxBytes int64
	// runTimeout bounds how long a single run may execute. Zero disables the cap.
	runTimeout time.Duration
	// noJanitor disables the stale-lease janitor. A relay worker sets it because the store it runs
	// against cannot reclaim leases; that stays the control node's job.
	noJanitor bool
	// presence records that this process is polling for work, nil when nothing records it.
	presence PresenceRecorder
	// claimGate refuses new claims while it returns an error, for a process whose claiming is a
	// licensed feature. Nil on a single node install, where claiming is not.
	claimGate func() error
	// now reads the wall clock for the timestamps a run carries on its record and its outcome entry:
	// created, started, ended. Nil defaults to time.Now. It exists so the demo can seed a run as of a
	// past instant, with its record, chain entry, and receipt all agreeing on that time. It never
	// governs lease or dedupe timing, which must read the real clock the store ages leases against.
	now func() time.Time
}

// WithDecisions keeps the record of each decision a person makes, its reason, its corrections, and
// its separation-of-duties evaluation, in store. A server passes its database's store so the
// records outlive the process; without one they live in memory.
func WithDecisions(store decision.Store) Option {
	return func(c *config) { c.decisions = store }
}

// WithWorkers sets the worker pool size. Values below one fall back to DefaultWorkers.
func WithWorkers(n int) Option {
	return func(c *config) { c.workers = n }
}

// WithRunTimeout caps how long a single run may execute. A run that exceeds it is canceled and
// finalized failed, so a hung tool cannot hold a worker slot forever. Zero or less disables the cap.
func WithRunTimeout(d time.Duration) Option {
	return func(c *config) {
		if d < 0 {
			d = 0
		}
		c.runTimeout = d
	}
}

// WithMaxShards sets the ceiling on how many groups a split fans out into. A value below one restores
// the default. A split is always bounded by the host count regardless.
func WithMaxShards(n int) Option {
	return func(c *config) { c.maxShards = n }
}

// WithAudits gives the dispatcher the audit chain, so it commits each run's outcome as a
// tamper-evident entry when the run finishes. Nil keeps no such record.
func WithAudits(audits audit.Store) Option {
	return func(c *config) { c.audits = audits }
}

// WithClock overrides the wall clock the dispatcher stamps run records and outcome entries from. The
// demo passes a clock parked in the past so a seeded run's created, started, and ended times, the
// audit entry that records its outcome, and the receipt built from that entry all agree on when it
// ran. A nil function restores time.Now. It does not move the clock leases or dedupe keys are aged
// against; those stay on the real time the store shares with every worker.
func WithClock(now func() time.Time) Option {
	return func(c *config) { c.now = now }
}

// WithPublisher sets the Publisher that receives live events and log chunks.
func WithPublisher(p Publisher) Option {
	return func(c *config) { c.publisher = p }
}

// WithOwner sets the name this process stamps on the runs it leases.
func WithOwner(owner string) Option {
	return func(c *config) { c.owner = owner }
}

// WithClaimInterval sets how often the claim loop polls the store when idle.
func WithClaimInterval(d time.Duration) Option {
	return func(c *config) { c.claimInterval = d }
}

// WithQueues sets the queues this process serves. A worker given named queues runs only work
// targeted at them; the default when unset is the empty default pool.
func WithQueues(queues []string) Option {
	return func(c *config) { c.queues = queues }
}

// WithRunFilesRoot sets the directory each run's private credential directory is created under,
// instead of the per-account directory in the system temporary directory.
func WithRunFilesRoot(dir string) Option {
	return func(c *config) { c.runFilesRoot = dir }
}

// WithNoJanitor disables the stale-lease janitor. A relay worker runs against a store that cannot
// reclaim leases, so it turns the sweep off and leaves stale-lease recovery to the control node.
func WithNoJanitor() Option {
	return func(c *config) { c.noJanitor = true }
}

// WithClaimGate sets a check the claim loop makes before leasing each run, and stops it claiming
// while the check refuses.
//
// It exists for the relay worker, whose whole reason to run is a paid feature. That feature was
// gated once at startup, and the process then blocks on a signal for as long as the operator leaves
// it up, so a term that lapsed a year ago still had a fleet of workers draining the queue. The
// evidence emitter had the same shape and now reads its license on every tick; this is that, for a
// loop rather than a ticker.
//
// It is an option rather than a check inside the loop because the same loop runs on a single node
// install, where claiming is not a paid feature and gating it would stop an unlicensed install from
// running anything at all. Only the worker passes one.
//
// A refusal stops new claims and nothing else. Runs already executing finish, the process stays up,
// and the operator's daemon is not killed underneath them, which is the same rule every other lapse
// follows.
func WithClaimGate(gate func() error) Option {
	return func(c *config) { c.claimGate = gate }
}

// WithNotifyClient replaces the client that delivers notifications.
//
// The default refuses to dial this server itself, because a notification target is named by whoever
// started the run. A test serving its own receiver on the loopback interface is the case that needs
// to opt out, and it opts out explicitly rather than the guard being loosened for everybody.
func WithNotifyClient(c *http.Client) Option {
	return func(cfg *config) { cfg.notifyClient = c }
}

// New returns a Dispatcher. It panics if store or runner is nil; a nil logger becomes a no-op.
func New(store run.Store, runner roundhouse.Runner, log *zap.Logger, opts ...Option) *Dispatcher {
	if store == nil {
		panic("dispatch: Store required")
	}
	if runner == nil {
		panic("dispatch: Runner required")
	}
	if log == nil {
		log = zap.NewNop()
	}

	cfg := config{workers: DefaultWorkers}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.workers < 1 {
		cfg.workers = DefaultWorkers
	}
	if cfg.maxShards < 1 {
		cfg.maxShards = DefaultMaxShards
	}
	if cfg.publisher == nil {
		cfg.publisher = noopPublisher{}
	}
	if cfg.owner == "" {
		cfg.owner = defaultOwner()
	}
	if cfg.claimInterval <= 0 {
		cfg.claimInterval = DefaultClaimInterval
	}
	if len(cfg.queues) == 0 {
		cfg.queues = []string{""}
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	if cfg.decisions == nil {
		cfg.decisions = decision.NewMemStore()
	}

	lister, _ := runner.(roundhouse.HostLister)
	dumper, _ := runner.(roundhouse.InventoryDumper)
	invLister, _ := runner.(roundhouse.InventoryLister)
	invReader, _ := runner.(roundhouse.InventoryReader)
	coreReporter, _ := runner.(roundhouse.AnsibleCoreReporter)
	ctx, cancel := context.WithCancelCause(context.Background())
	d := &Dispatcher{
		store:               store,
		audits:              cfg.audits,
		notifyHTTP:          cfg.notifyClient,
		runner:              runner,
		log:                 log,
		sem:                 make(chan struct{}, cfg.workers),
		ctx:                 ctx,
		cancel:              cancel,
		publisher:           cfg.publisher,
		hostLister:          lister,
		invLister:           invLister,
		invReader:           invReader,
		ansibleCoreReporter: coreReporter,
		dumper:              dumper,
		cancels:             make(map[string]context.CancelCauseFunc),
		coordinators:        make(map[string]chan struct{}),
		owner:               cfg.owner,
		claimInterval:       cfg.claimInterval,
		wakeCh:              make(chan struct{}, 1),
		runTimeout:          cfg.runTimeout,
		now:                 cfg.now,
		maxShards:           cfg.maxShards,
		queues:              cfg.queues,
		credentials:         cfg.credentials,
		credentialTypes:     cfg.credentialTypes,
		federation:          cfg.federation,
		runFilesRoot:        cfg.runFilesRoot,
		sealer:              cfg.sealer,
		delivery:            cfg.delivery,
		projects:            cfg.projects,
		syncer:              cfg.syncer,
		webhooks:            cfg.webhooks,
		slackWebhooks:       cfg.slackWebhooks,
		mattermostWebhooks:  cfg.mattermostWebhooks,
		rocketChatWebhooks:  cfg.rocketChatWebhooks,
		discordWebhooks:     cfg.discordWebhooks,
		teamsWebhooks:       cfg.teamsWebhooks,
		ntfyURLs:            cfg.ntfyURLs,
		ntfyToken:           cfg.ntfyToken,
		pagerdutyKeys:       cfg.pagerdutyKeys,
		grafanaURLs:         cfg.grafanaURLs,
		grafanaToken:        cfg.grafanaToken,
		twilioSID:           cfg.twilioSID,
		twilioToken:         cfg.twilioToken,
		twilioFrom:          cfg.twilioFrom,
		twilioTo:            cfg.twilioTo,
		pagerDutyEndpoint:   defaultPagerDutyEndpoint,
		twilioBaseURL:       defaultTwilioBaseURL,
		emailer:             cfg.emailer,
		emailOnFailureOnly:  cfg.emailOnFailureOnly,
		router:              cfg.router,
		outbox:              cfg.outbox,
		inventories:         cfg.inventories,
		invSources:          cfg.invSources,
		factCache:           cfg.factCache,
		syncSources:         cfg.syncSources,
		policies:            cfg.policies,
		decisions:           cfg.decisions,
		defaultImage:        cfg.defaultImage,
		imageResolver:       cfg.imageResolver,
		claimGate:           cfg.claimGate,
		presence:            cfg.presence,
		planScans:           newPlanScanCache(),
	}
	d.modules = newModuleStore(d.runFiles(), cmp.Or(cfg.moduleKeepFor, DefaultModuleKeepFor),
		cmp.Or(cfg.moduleKeepMaxBytes, DefaultModuleKeepMaxBytes))
	d.moduleFetchTimeout, d.moduleFetchMaxBytes = DefaultModuleFetchTimeout, DefaultModuleFetchMaxBytes
	if cfg.moduleFetchTimeout > 0 {
		d.moduleFetchTimeout = cfg.moduleFetchTimeout
	}
	if cfg.moduleFetchMaxBytes > 0 {
		d.moduleFetchMaxBytes = cfg.moduleFetchMaxBytes
	}
	// Every executor sweeps, whether or not it holds credentials, because the fact cache, the
	// secrets a relay worker is delivered, and the runner's own files live in run directories too.
	// The root is read here, once, so the sweep and the runs agree on it.
	d.wg.Add(1)
	go d.sweepRunFiles(runfiles.NewSweeper(d.runFiles()))
	d.startOutbox()
	d.wg.Add(1)
	go d.claimLoop()
	if !cfg.noJanitor {
		d.wg.Add(1)
		go d.janitor()
	}
	if d.syncSources && d.invSources != nil {
		d.wg.Add(1)
		go d.sourceSyncLoop()
	}
	if d.presence != nil {
		d.wg.Add(1)
		go d.presenceLoop()
	}
	if slots, ok := store.(slotReporter); ok {
		slots.SetClaimSlots(cfg.workers)
	}
	return d
}

// defaultOwner builds a lease owner name from the host and process so leases are attributable.
func defaultOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "switchtender"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

// Owner returns the name this process stamps on its leases.
func (d *Dispatcher) Owner() string {
	return d.owner
}
