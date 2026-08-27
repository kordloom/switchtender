package dispatch

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"go.uber.org/zap"
)

// Option configures a Dispatcher.
type Option func(*config)

// config holds optional Dispatcher settings before construction.
type config struct {
	// notifyClient dials notification targets. Nil uses the guarded default, which refuses this
	// server itself; a test serving on loopback sets its own.
	notifyClient *http.Client
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
	// sealer decrypts credential secrets.
	sealer *credential.Sealer
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
	// inventories resolves stored inventories, nil when the feature is off.
	inventories inventory.Store
	// invSources resolves dynamic inventory sources, nil when the feature is off.
	invSources invsource.Store
	// syncSources enables the background scheduled-sync loop for dynamic inventory sources.
	syncSources bool
	// policies gate submitted runs by holding matches for approval, nil when enforcement is off.
	policies policy.Store
	// defaultImage is the fallback execution image used when a run, its template, and its project pin
	// none. Empty leaves an unpinned run on the host.
	defaultImage string
	// runTimeout bounds how long a single run may execute. Zero disables the cap.
	runTimeout time.Duration
	// noJanitor disables the stale-lease janitor. A relay worker sets it because the store it runs
	// against cannot reclaim leases; that stays the control node's job.
	noJanitor bool
	// now reads the wall clock for the timestamps a run carries on its record and its outcome entry:
	// created, started, ended. Nil defaults to time.Now. It exists so the demo can seed a run as of a
	// past instant, with its record, chain entry, and receipt all agreeing on that time. It never
	// governs lease or dedupe timing, which must read the real clock the store ages leases against.
	now func() time.Time
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

// WithNoJanitor disables the stale-lease janitor. A relay worker runs against a store that cannot
// reclaim leases, so it turns the sweep off and leaves stale-lease recovery to the control node.
func WithNoJanitor() Option {
	return func(c *config) { c.noJanitor = true }
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

	lister, _ := runner.(roundhouse.HostLister)
	dumper, _ := runner.(roundhouse.InventoryDumper)
	ctx, cancel := context.WithCancelCause(context.Background())
	d := &Dispatcher{
		store:              store,
		audits:             cfg.audits,
		notifyHTTP:         cfg.notifyClient,
		runner:             runner,
		log:                log,
		sem:                make(chan struct{}, cfg.workers),
		ctx:                ctx,
		cancel:             cancel,
		publisher:          cfg.publisher,
		hostLister:         lister,
		dumper:             dumper,
		cancels:            make(map[string]context.CancelFunc),
		owner:              cfg.owner,
		claimInterval:      cfg.claimInterval,
		wakeCh:             make(chan struct{}, 1),
		runTimeout:         cfg.runTimeout,
		now:                cfg.now,
		maxShards:          cfg.maxShards,
		queues:             cfg.queues,
		credentials:        cfg.credentials,
		credentialTypes:    cfg.credentialTypes,
		sealer:             cfg.sealer,
		projects:           cfg.projects,
		syncer:             cfg.syncer,
		webhooks:           cfg.webhooks,
		slackWebhooks:      cfg.slackWebhooks,
		mattermostWebhooks: cfg.mattermostWebhooks,
		rocketChatWebhooks: cfg.rocketChatWebhooks,
		discordWebhooks:    cfg.discordWebhooks,
		teamsWebhooks:      cfg.teamsWebhooks,
		ntfyURLs:           cfg.ntfyURLs,
		ntfyToken:          cfg.ntfyToken,
		pagerdutyKeys:      cfg.pagerdutyKeys,
		grafanaURLs:        cfg.grafanaURLs,
		grafanaToken:       cfg.grafanaToken,
		twilioSID:          cfg.twilioSID,
		twilioToken:        cfg.twilioToken,
		twilioFrom:         cfg.twilioFrom,
		twilioTo:           cfg.twilioTo,
		pagerDutyEndpoint:  defaultPagerDutyEndpoint,
		twilioBaseURL:      defaultTwilioBaseURL,
		emailer:            cfg.emailer,
		emailOnFailureOnly: cfg.emailOnFailureOnly,
		inventories:        cfg.inventories,
		invSources:         cfg.invSources,
		syncSources:        cfg.syncSources,
		policies:           cfg.policies,
		defaultImage:       cfg.defaultImage,
	}
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
