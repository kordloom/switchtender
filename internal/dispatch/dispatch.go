// Package dispatch orchestrates run execution: it accepts run requests, schedules them across a
// bounded worker pool, drives status transitions, and streams output into the store.
package dispatch

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// DefaultWorkers is the number of concurrent runs when none is configured.
const DefaultWorkers = 4

// tailPollInterval is how often the event tailer checks the sidecar file for new lines.
const tailPollInterval = 75 * time.Millisecond

// costWindow is how many recent runs per host feed the average duration used to balance splits.
const costWindow = 5

const (
	// DefaultClaimInterval is how often an idle executor polls the store for pending runs.
	DefaultClaimInterval = 250 * time.Millisecond
	// watchInterval is how often an executing run renews its lease and checks for a cancel
	// request from another process.
	watchInterval = 3 * time.Second
	// leaseTTL is how stale a lease may grow before the janitor treats its holder as dead.
	leaseTTL = 30 * time.Second
	// summaryPageSize is how many events are read at a time when folding a finished run's summaries.
	// It bounds peak memory at completion, which is when several runs tend to finish at once.
	summaryPageSize = 5000
	// janitorInterval is how often stale leases are swept.
	janitorInterval = 10 * time.Second
	// idleBackoffShift is how many times an idle claim wait may double, so the ceiling is the claim
	// interval shifted by it. A dispatcher with nothing to claim backs off toward that ceiling
	// rather than hammering the store, and drops back to the base interval the moment it claims.
	idleBackoffShift = 3
	// dedupeRetryShards names the shard-retry action in the idempotency keys it dedupes under.
	dedupeRetryShards = "retry-shards"
)

// Publisher receives live run output for streaming to clients. All methods must be safe for
// concurrent use and must not block.
type Publisher interface {
	// PublishEvents delivers newly parsed events for a run.
	PublishEvents(id string, events []event.Event)
	// PublishLog delivers a newly written log chunk for a run.
	PublishLog(id string, chunk []byte)
	// CloseRun signals that a run has finished producing output.
	CloseRun(id string)
}

// noopPublisher discards all output. It is the default when no Publisher is configured.
type noopPublisher struct{}

// PublishEvents discards events.
func (noopPublisher) PublishEvents(string, []event.Event) {}

// PublishLog discards a log chunk.
func (noopPublisher) PublishLog(string, []byte) {}

// CloseRun does nothing.
func (noopPublisher) CloseRun(string) {}

// Dispatcher accepts run requests and executes them across a bounded worker pool.
type Dispatcher struct {
	// store persists runs and their output.
	store run.Store
	// runner executes a single playbook.
	runner roundhouse.Runner
	// log records dispatcher activity.
	log *zap.Logger
	// sem bounds the number of concurrently executing runs.
	sem chan struct{}
	// wg tracks in-flight workers so Close can wait for them.
	wg sync.WaitGroup
	// notifyWG tracks in-flight webhook and email deliveries so Close waits for them to finish
	// instead of cutting them off mid-send.
	notifyWG sync.WaitGroup
	// ctx is canceled by Close to stop in-flight and pending runs.
	ctx context.Context
	// cancel cancels ctx.
	cancel context.CancelFunc
	// publisher receives live output for streaming.
	publisher Publisher
	// hostLister enumerates inventory hosts for split runs, nil when the runner cannot list hosts.
	hostLister roundhouse.HostLister
	// cmu guards cancels.
	cmu sync.Mutex
	// cancels maps a pending or executing run id to its cancel func.
	cancels map[string]context.CancelFunc
	// owner identifies this process on the leases it takes.
	owner string
	// claimInterval is how often the claim loop polls when idle.
	claimInterval time.Duration
	// wakeCh nudges the idle claim loop to poll now instead of finishing its backoff. It holds one
	// token, because one pending token already means "there is work, poll again".
	wakeCh chan struct{}
	// maxShards caps how many groups a split fans out into.
	maxShards int
	// queues names the queues this process serves; empty serves the default pool.
	queues []string
	// credentials resolves stored execution secrets, nil when the feature is off.
	credentials credential.Store
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
	// syncSources runs the background scheduled-sync loop for dynamic inventory sources.
	syncSources bool
	// dumper renders inventory sources to JSON.
	dumper roundhouse.InventoryDumper
	// policies gate submitted runs by holding matches for approval, nil when enforcement is off.
	policies policy.Store
	// defaultImage is the fallback execution image used when a run, its template, and its project pin
	// none. Empty leaves an unpinned run on the host.
	defaultImage string
	// runTimeout bounds how long a single run may execute before it is canceled and finalized failed.
	// Zero disables the cap, so a run may take as long as it needs.
	runTimeout time.Duration
}

// errRunTimeout is the cancellation cause when a run is stopped for exceeding runTimeout, so the
// outcome can record a timeout rather than a user cancel.
var errRunTimeout = errors.New("run exceeded its timeout")

// Option configures a Dispatcher.
type Option func(*config)

// config holds optional Dispatcher settings before construction.
type config struct {
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

	lister, _ := runner.(roundhouse.HostLister)
	dumper, _ := runner.(roundhouse.InventoryDumper)
	ctx, cancel := context.WithCancel(context.Background())
	d := &Dispatcher{
		store:              store,
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
		maxShards:          cfg.maxShards,
		queues:             cfg.queues,
		credentials:        cfg.credentials,
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

// claimLoop leases pending runs from the store and executes them, one claim per free worker slot,
// until the dispatcher closes. Every process running a dispatcher takes part, so a lone server
// executes its own queue and added workers simply compete for the same leases.
func (d *Dispatcher) claimLoop() {
	defer d.wg.Done()
	idle := 0
	for {
		select {
		case d.sem <- struct{}{}:
		case <-d.ctx.Done():
			return
		}

		r, err := d.store.Claim(d.ctx, d.owner, d.queues)
		if err != nil {
			<-d.sem
			if !errors.Is(err, run.ErrNonePending) && d.ctx.Err() == nil {
				d.log.Error("dispatch: claim: " + err.Error())
			}
			idle++
			timer := time.NewTimer(d.idleWait(idle))
			select {
			case <-timer.C:
			case <-d.wakeCh:
				// Work arrived. Start over at the base interval, since a controller that just
				// took a submission is not idle any more.
				timer.Stop()
				idle = 0
			case <-d.ctx.Done():
				timer.Stop()
				return
			}
			continue
		}
		idle = 0

		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			defer func() { <-d.sem }()
			d.executeLeased(d.ctx, r)
		}()
	}
}

// wake asks the claim loop to poll immediately rather than wait out its idle backoff.
//
// The backoff exists so idle dispatchers stop competing for the store's single writer, but nothing
// told the loop when work arrived, so its whole wait landed on submit-to-start latency: a run
// submitted to a quiet controller waited a measured 1.7 seconds on average and 2.75 at worst, from
// 250ms before the backoff existed. A submitting caller signals here and the loop starts at once,
// which keeps the backoff's benefit without paying for it on the first run after an idle spell.
//
// It never blocks and it is safe to call for a run that turns out not to be claimable. A spurious
// wake costs one empty claim, after which the loop backs off again; a missed wake costs a user
// seconds of waiting. Over-signaling is deliberately the cheaper mistake, which is why every submit
// path calls this rather than only the ones that provably created claimable work.
func (d *Dispatcher) wake() {
	select {
	case d.wakeCh <- struct{}{}:
	default:
	}
}

// idleWait returns how long to wait before the next claim, given how many consecutive claims came
// back empty. The wait doubles toward a ceiling so a dispatcher with nothing to do stops competing
// for the store's single writer with the runs that are actually executing, and it carries jitter so
// several dispatchers sharing a store spread their polls out instead of arriving together. One
// claim resets the count, so work is never picked up on a stale backoff.
func (d *Dispatcher) idleWait(idle int) time.Duration {
	wait := d.claimInterval << min(max(idle-1, 0), idleBackoffShift)
	// Half the wait, plus a random share of it: the mean stays at wait and no two idle dispatchers
	// stay in step.
	return wait/2 + time.Duration(rand.Int64N(int64(wait)))
}

// janitor sweeps stale leases so runs owned by dead processes requeue or resolve. It runs once
// immediately, covering restarts, then on an interval.
func (d *Dispatcher) janitor() {
	defer d.wg.Done()
	sweep := func() {
		n, err := d.store.ReclaimStale(d.ctx, leaseTTL)
		if err != nil {
			if d.ctx.Err() == nil {
				d.log.Error("dispatch: reclaim stale: " + err.Error())
			}
			return
		}
		if n > 0 {
			d.log.Info("dispatch: reclaimed stale runs", zap.Int("count", n))
		}
	}
	sweep()
	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

// validateRun checks a run's credential and project references before it is accepted.
func (d *Dispatcher) validateRun(ctx context.Context, r *run.Run) error {
	if err := d.validateCredentials(ctx, r.Tool, r.CredentialIDs); err != nil {
		return err
	}
	if err := d.validateInventory(ctx, r.InventoryID); err != nil {
		return err
	}
	return d.validateProject(ctx, r.ProjectID)
}

// resolveQueue fills a run's queue from its stored inventory when neither the request nor its
// template pinned one, so an inventory can pin all of its work to a worker group. The precedence
// is run, then template (already applied as the run's queue by launch), then inventory. A lookup
// failure leaves the queue empty rather than failing the submit, since validateRun has already
// confirmed the inventory exists.
func (d *Dispatcher) resolveQueue(ctx context.Context, r *run.Run) {
	if r.Queue != "" || r.InventoryID == "" || d.inventories == nil {
		return
	}
	inv, err := d.inventories.Get(ctx, r.InventoryID)
	if err != nil {
		return
	}
	r.Queue = inv.Queue
}

// requireToolInput checks that a run carries the input its tool needs: a playbook for Ansible, a
// command for bash, terraform, and python. It also rejects a run naming an unsupported tool.
func requireToolInput(r *run.Run) error {
	if !run.ValidTool(r.Tool) {
		return ErrUnknownTool
	}
	if run.NormalizeTool(r.Tool) == run.ToolAnsible {
		if r.Playbook == "" {
			return ErrNoPlaybook
		}
		return nil
	}
	if r.Command == "" {
		return ErrNoCommand
	}
	return nil
}

// requireStepInput checks that a pipeline step carries the input its tool needs, mirroring
// requireToolInput for a step.
func requireStepInput(s run.PipelineStep) error {
	if !run.ValidTool(s.Tool) {
		return ErrUnknownTool
	}
	if run.NormalizeTool(s.Tool) == run.ToolAnsible {
		if s.Playbook == "" {
			return ErrNoPlaybook
		}
		return nil
	}
	if s.Command == "" {
		return ErrNoCommand
	}
	return nil
}

// idempotentLookup returns the run already recorded under key, or nil when the key is empty or
// unused. A retried submission carrying a key a prior submission already used resolves to that
// original run, so the retry never fires a second run.
func (d *Dispatcher) idempotentLookup(ctx context.Context, key string) (*run.Run, error) {
	if key == "" {
		return nil, nil
	}
	existing, err := d.store.ByIdempotencyKey(ctx, key)
	if errors.Is(err, run.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return existing, nil
}

// idempotentSave persists a newly built run and settles the concurrent-retry race the pre-check
// cannot. When another submission claimed the same key between the lookup and this save, the store's
// unique index rejects r with run.ErrDuplicateKey; the winning run is fetched and returned with dup
// true so the caller returns it and skips any follow-on work such as spawning children. Without a
// key it is an ordinary save.
func (d *Dispatcher) idempotentSave(ctx context.Context, r *run.Run) (result *run.Run, dup bool, err error) {
	saveErr := d.store.Save(ctx, r)
	if errors.Is(saveErr, run.ErrDuplicateKey) {
		winner, ferr := d.store.ByIdempotencyKey(ctx, r.IdempotencyKey)
		if ferr != nil {
			return nil, false, ferr
		}
		return winner, true, nil
	}
	if saveErr != nil {
		return nil, false, saveErr
	}
	return r, false, nil
}

// Submit accepts a run for a tool against inventory and returns the created run in pending state.
// Execution proceeds asynchronously; callers observe progress through the store. A submission
// carrying an idempotency key that a prior submit already used returns that original run untouched.
func (d *Dispatcher) Submit(ctx context.Context, playbook, inventory string, opts ...run.SubmitOption) (*run.Run, error) {
	r := &run.Run{
		ID:        run.NewID(),
		Playbook:  playbook,
		Inventory: inventory,
		Status:    run.StatusPending,
		CreatedAt: time.Now(),
	}
	run.ApplyOptions(r, opts)
	if err := requireToolInput(r); err != nil {
		return nil, err
	}
	if existing, err := d.idempotentLookup(ctx, r.IdempotencyKey); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	if err := d.validateRun(ctx, r); err != nil {
		return nil, err
	}
	d.resolveQueue(ctx, r)
	if r.Status != run.StatusPendingApproval {
		held, perr := d.requiresApproval(ctx, r)
		if perr != nil {
			return nil, perr
		}
		if held {
			r.Status = run.StatusPendingApproval
		}
	}
	created, _, err := d.idempotentSave(ctx, r)
	if err != nil {
		return nil, err
	}
	// Execution happens through the claim loop, here or in any worker sharing the store, so the
	// local loop is nudged rather than left to finish an idle backoff a user would wait out.
	d.wake()
	return created, nil
}

// SubmitSplit shards a run across the inventory and returns the parent run in pending state. Each
// shard runs the same playbook limited to its slice of hosts, and the parent rolls up their result.
// Hosts are packed into shards by their average duration in recent runs so each shard carries a
// similar amount of work; hosts without history balance by count. When shards is below two or the
// inventory has fewer than two hosts, it falls back to a single run.
func (d *Dispatcher) SubmitSplit(ctx context.Context, playbook, inventory string, shards int, opts ...run.SubmitOption) (*run.Run, error) {
	// Sharding fans a playbook across inventory hosts, which only Ansible does; other tools run once.
	probe := &run.Run{}
	run.ApplyOptions(probe, opts)
	// A retried split returns the original parent without re-listing hosts or resharding.
	if existing, err := d.idempotentLookup(ctx, probe.IdempotencyKey); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	if run.NormalizeTool(probe.Tool) != run.ToolAnsible {
		return d.Submit(ctx, playbook, inventory, opts...)
	}
	if playbook == "" {
		return nil, ErrNoPlaybook
	}
	if shards < 2 {
		return d.Submit(ctx, playbook, inventory, opts...)
	}
	if d.hostLister == nil {
		return nil, ErrNoHostLister
	}

	// A stored inventory must exist as a file before its hosts can be enumerated for sharding.
	listPath := inventory
	if probe.InventoryID != "" {
		path, cleanup, _, err := d.inventoryFile(ctx, probe.InventoryID)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		listPath = path
	}
	hosts, err := d.hostLister.Hosts(ctx, listPath)
	if err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}
	if len(hosts) < 2 {
		return d.Submit(ctx, playbook, inventory, opts...)
	}

	costs, err := d.store.HostCosts(ctx, costWindow)
	if err != nil {
		d.log.Warn("dispatch: host costs unavailable: balancing by host count: " + err.Error())
		costs = nil
	}

	groups := partition(hosts, min(shards, d.maxShards), costs)
	count := len(groups)
	parent := &run.Run{
		ID: run.NewID(), Playbook: playbook, Inventory: inventory, Kind: run.KindSplit,
		Status: run.StatusPending, CreatedAt: time.Now(), ShardCount: &count,
	}
	run.ApplyOptions(parent, opts)
	if err := d.validateRun(ctx, parent); err != nil {
		return nil, err
	}
	d.resolveQueue(ctx, parent)
	// A split is submitted through a different path than a single run, so without this the same
	// command an operator gated ran freely by being sharded: the identical playbook that Submit
	// held for an approver executed on every host the moment it was split in two. A shard matches
	// exactly what its parent matches, since it inherits everything but its host group, so the
	// parent is the only thing worth testing.
	if parent.Status != run.StatusPendingApproval {
		held, perr := d.requiresApproval(ctx, parent)
		if perr != nil {
			return nil, perr
		}
		if held {
			parent.Status = run.StatusPendingApproval
		}
	}
	created, dup, err := d.idempotentSave(ctx, parent)
	if err != nil {
		return nil, err
	}
	if dup {
		// A concurrent submission won the key; return its parent and create no children here.
		return created, nil
	}

	// Shards of a held split are stored held too. Nothing can claim them in that state, and they
	// have to exist before the approval so the decision covers the shards an approver was shown and
	// so the split survives a restart while it waits.
	childStatus := run.StatusPending
	if parent.Status == run.StatusPendingApproval {
		childStatus = run.StatusPendingApproval
	}
	parentID := parent.ID
	children := make([]*run.Run, 0, count)
	for i, group := range groups {
		idx, shardCount := i, count
		child := &run.Run{
			ID: run.NewID(), Playbook: playbook, Inventory: inventory,
			Status: childStatus, CreatedAt: time.Now(),
			ParentID: &parentID, ShardIndex: &idx, ShardCount: &shardCount,
			Limit: strings.Join(group, ","),
		}
		// A shard is the parent run over a subset of its hosts, so it executes the same way. Copying
		// a chosen few fields meant a split silently dropped the rest: extra vars vanished, shards
		// ran outside the execution image the parent pinned, and the run timeout did not apply.
		inheritExecution(child, parent)
		if err := d.store.Save(ctx, child); err != nil {
			return nil, err
		}
		children = append(children, child)
	}

	if parent.Status == run.StatusPendingApproval {
		// Held for an approver. Approve starts the coordinator, since no claim loop takes a parent.
		return parent, nil
	}

	d.wake()
	d.wg.Add(1)
	go d.coordinate(parent.Clone(), children)

	return parent, nil
}

// inheritExecution copies onto child every field that decides how a run executes, so a shard of a
// split, or a retry of one, runs exactly the way its parent would have.
//
// The fields come from run.Run.ExecutionOptions rather than a list kept here. They were copied by
// hand in several places and every list fell behind the run model: a split lost its extra vars, ran
// outside the execution image its parent pinned, and ignored the parent's timeout, while a rerun
// lost the timeout and the notifications. Anything added to run.Run that changes how a run executes
// belongs in ExecutionOptions, where every path derived from a run reads it.
func inheritExecution(child, parent *run.Run) {
	for _, opt := range parent.ExecutionOptions() {
		opt(child)
	}
}

// RetryFailedShards creates and starts a new split run that re-runs only the failed shards of a
// finished split parent, keeping each failed shard's host group. Shards that succeeded do not run
// again. The new parent links back to the run it retries through RetryOf. Retrying the same parent
// twice inside the dedupe window returns the first retry, so a double click cannot fire two.
func (d *Dispatcher) RetryFailedShards(ctx context.Context, parentID string) (*run.Run, error) {
	existing, key, err := run.ResolveDedupe(ctx, d.store, dedupeRetryShards, parentID, time.Now())
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}
	parent, err := d.store.Get(ctx, parentID)
	if err != nil {
		return nil, err
	}
	if parent.Kind != run.KindSplit {
		return nil, ErrNotSplit
	}
	if !parent.Status.Terminal() {
		return nil, ErrNotFinished
	}

	shards, err := d.store.Shards(ctx, parentID)
	if err != nil {
		return nil, fmt.Errorf("list shards: %w", err)
	}
	var failed []*run.Run
	// Only a shard that actually ran and failed is worth running again. A canceled shard did not
	// fail, it was stopped, and treating it as failed is what turned a rejection into a retryable
	// set: rejecting a split cancels its held shards, which then read as failures.
	for _, s := range shards {
		if s.Status == run.StatusFailed || s.Status == run.StatusInterrupted {
			failed = append(failed, s)
		}
	}
	if len(failed) == 0 {
		return nil, ErrNoFailedShards
	}

	count := len(failed)
	retry := &run.Run{
		ID: run.NewID(), Playbook: parent.Playbook, Inventory: parent.Inventory,
		Kind: run.KindSplit, Status: run.StatusPending, CreatedAt: time.Now(),
		ShardCount: &count, RetryOf: &parent.ID, IdempotencyKey: key,
	}
	inheritExecution(retry, parent)
	// A retry is a fourth way to submit a run, and it inherits the parent's entire execution spec,
	// so it has to face the same gate as the other three. Submit, SubmitSplit, and SubmitPipeline
	// each consult the policy; this path did not, which made retrying a way to run a spec an
	// approver would have held.
	held, perr := d.requiresApproval(ctx, retry)
	if perr != nil {
		return nil, perr
	}
	if held {
		retry.Status = run.StatusPendingApproval
	}
	saved, dup, err := d.idempotentSave(ctx, retry)
	if err != nil {
		return nil, err
	}
	if dup {
		// A concurrent click won the key; return its retry and create no second set of shards.
		return saved, nil
	}

	retryChildStatus := run.StatusPending
	if retry.Status == run.StatusPendingApproval {
		retryChildStatus = run.StatusPendingApproval
	}
	retryID := retry.ID
	children := make([]*run.Run, 0, count)
	for i, shard := range failed {
		idx, shardCount := i, count
		child := &run.Run{
			ID: run.NewID(), Playbook: retry.Playbook, Inventory: retry.Inventory,
			Status: retryChildStatus, CreatedAt: time.Now(),
			ParentID: &retryID, ShardIndex: &idx, ShardCount: &shardCount,
			// The host group is the one thing a shard owns; everything about how it executes comes
			// from the run it is a shard of.
			Limit: shard.Limit,
		}
		inheritExecution(child, retry)
		if err := d.store.Save(ctx, child); err != nil {
			return nil, err
		}
		children = append(children, child)
	}

	if retry.Status == run.StatusPendingApproval {
		// Held for an approver. Approve starts the coordinator, since no claim loop takes a parent.
		return retry, nil
	}
	d.wake()
	d.wg.Add(1)
	go d.coordinate(retry.Clone(), children)

	return retry, nil
}

// idsOf returns the ids of a set of runs.
func idsOf(runs []*run.Run) []string {
	out := make([]string, len(runs))
	for i, r := range runs {
		out[i] = r.ID
	}
	return out
}

// coordinate waits for the parent's shards, which execute wherever a claim loop picks them up,
// and finalizes the parent from their stored results. The parent carries this process's lease so
// a dead coordinator is swept, and canceling the parent propagates through the store to every
// shard no matter which process holds it.
func (d *Dispatcher) coordinate(parent *run.Run, children []*run.Run) {
	defer d.wg.Done()

	parentCtx, cancelParent := context.WithCancel(d.ctx)
	d.register(parent.ID, cancelParent)
	defer d.unregister(parent.ID)
	defer cancelParent()

	// A parent that already reached a terminal state is not started.
	//
	// This save was unconditional, and it is an upsert, so a parent canceled between submit and
	// this line came back running and the whole fan-out proceeded. finalize goes through a fence for
	// exactly this reason; the start had none. Reading the stored status first is the fence: it
	// costs one point read per parent and closes the window where a cancel is silently undone.
	if current, err := d.store.Get(context.Background(), parent.ID); err == nil {
		if current.Status.Terminal() {
			d.log.Info("dispatch: parent already finished, not starting its coordination",
				zap.String("run_id", parent.ID), zap.String("status", string(current.Status)))
			d.cancelChildren(idsOf(children))
			return
		}
		if current.CancelRequested {
			d.log.Info("dispatch: parent was canceled before it started",
				zap.String("run_id", parent.ID))
			d.cancelChildren(idsOf(children))
			d.finalize(parent, run.StatusCanceled, nil, "")
			return
		}
	}

	started := time.Now()
	parent.Status = run.StatusRunning
	parent.StartedAt = &started
	parent.ClaimedBy = d.owner
	parent.ClaimedAt = &started
	d.save(parent)

	watchCtx, stopWatch := context.WithCancel(parentCtx)
	defer stopWatch()
	go d.watch(watchCtx, parent.ID)

	ids := make([]string, len(children))
	for i, c := range children {
		ids[i] = c.ID
	}
	statuses := d.waitChildren(parentCtx, ids)

	allSucceeded := true
	anyCanceled := false
	for _, status := range statuses {
		if status != run.StatusSucceeded {
			allSucceeded = false
		}
		if status == run.StatusCanceled {
			anyCanceled = true
		}
	}
	switch {
	case allSucceeded:
		code := 0
		d.finalize(parent, run.StatusSucceeded, &code, "")
	case anyCanceled:
		d.finalize(parent, run.StatusCanceled, nil, "")
	default:
		code := 1
		d.finalize(parent, run.StatusFailed, &code, "")
	}
	d.publisher.CloseRun(parent.ID)
}

// childPollInterval is how often a coordinator checks its children's stored states.
const childPollInterval = 500 * time.Millisecond

// waitChildren polls the store until every child reaches a terminal state and returns their
// statuses in order. Reads use ctx, so a shutdown or a canceled parent stops the poll promptly
// rather than spinning against a closing store. On cancellation the children are cancel-requested
// through the store, any that no executor has claimed yet are finalized canceled directly, and any
// child not yet terminal is reported canceled, the honest summary for an interrupted parent.
func (d *Dispatcher) waitChildren(ctx context.Context, ids []string) []run.Status {
	statuses := make([]run.Status, len(ids))
	canceled := false
	parent := ""
	for {
		byID := d.childStatuses(ctx, ids, &parent)
		done := 0
		for i, id := range ids {
			if statuses[i].Terminal() {
				done++
				continue
			}
			status, ok := byID[id]
			if !ok {
				continue
			}
			if ctx.Err() != nil && !canceled {
				continue
			}
			if status.Terminal() {
				statuses[i] = status
				done++
			}
		}
		if done == len(ids) {
			return statuses
		}
		if ctx.Err() != nil && !canceled {
			canceled = true
			d.cancelChildren(ids)
		}
		select {
		case <-time.After(childPollInterval):
		case <-ctx.Done():
			// Shutting down or the parent was canceled: request cancellation once, then stop waiting
			// instead of polling a store that may be closing. Children still running are reported
			// canceled so the parent finalizes as canceled.
			if !canceled {
				d.cancelChildren(ids)
			}
			for i := range statuses {
				if !statuses[i].Terminal() {
					statuses[i] = run.StatusCanceled
				}
			}
			return statuses
		}
	}
}

// childStatuses reads the current status of the tracked children. A single child is a point read;
// a wider set resolves the shared parent once, then reads all children in one parent-scoped query
// per tick, so a 512-shard split does not issue hundreds of point reads every poll interval. When
// the parent-scoped read fails it falls back to point reads for that tick.
func (d *Dispatcher) childStatuses(ctx context.Context, ids []string, parent *string) map[string]run.Status {
	out := make(map[string]run.Status, len(ids))
	pointReads := func() {
		for _, id := range ids {
			if r, err := d.store.Get(ctx, id); err == nil {
				out[id] = r.Status
			}
		}
	}
	if len(ids) == 1 {
		pointReads()
		return out
	}
	if *parent == "" {
		r, err := d.store.Get(ctx, ids[0])
		if err != nil || r.ParentID == nil {
			pointReads()
			return out
		}
		*parent = *r.ParentID
	}
	children, err := d.store.Shards(ctx, *parent)
	if err != nil {
		pointReads()
		return out
	}
	for _, c := range children {
		out[c.ID] = c.Status
	}
	return out
}

// cancelChildren asks every non-terminal child to stop: claimed children through their executor's
// cancel watch, unclaimed ones finalized canceled directly since no executor will ever run them.
func (d *Dispatcher) cancelChildren(ids []string) {
	for _, id := range ids {
		r, err := d.store.Get(context.Background(), id)
		if err != nil || r.Status.Terminal() {
			continue
		}
		if err := d.store.RequestCancel(context.Background(), id); err != nil {
			d.log.Warn("dispatch: request child cancel: "+err.Error(), zap.String("run_id", id))
		}
		d.Cancel(id)
		if r.Status == run.StatusPending && r.ClaimedBy == "" {
			d.finalize(r, run.StatusCanceled, nil, "")
		}
	}
}

// SubmitPipeline runs playbook steps as one pipeline and returns the parent run in pending state.
// Each step is a child run, so it gets the full matrix, events, and cross run treatment. Steps run
// in order, or as a dependency graph when any step declares depends_on. A step that fails stops
// what follows or depends on it unless the step is marked continue on failure.
func (d *Dispatcher) SubmitPipeline(ctx context.Context, name, inventory string, steps []run.PipelineStep, opts ...run.SubmitOption) (*run.Run, error) {
	if len(steps) == 0 {
		return nil, ErrNoSteps
	}
	for _, s := range steps {
		if err := requireStepInput(s); err != nil {
			return nil, err
		}
	}
	if hasDependencies(steps) {
		if err := validateDAG(steps); err != nil {
			return nil, err
		}
	}

	parent := &run.Run{
		ID: run.NewID(), Playbook: name, Inventory: inventory, Kind: run.KindPipeline,
		Status: run.StatusPending, CreatedAt: time.Now(),
	}
	run.ApplyOptions(parent, opts)
	// The graph is stored on the parent so a pipeline held for approval can still be executed after
	// a restart, and so a finished pipeline can show the shape it ran.
	parent.Steps = steps
	// A retried pipeline returns the original parent instead of running its steps a second time.
	if existing, err := d.idempotentLookup(ctx, parent.IdempotencyKey); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	if err := d.validateRun(ctx, parent); err != nil {
		return nil, err
	}
	d.resolveQueue(ctx, parent)
	if parent.Status != run.StatusPendingApproval {
		held, perr := d.pipelineRequiresApproval(ctx, parent, steps)
		if perr != nil {
			return nil, perr
		}
		if held {
			parent.Status = run.StatusPendingApproval
		}
	}
	created, dup, err := d.idempotentSave(ctx, parent)
	if err != nil {
		return nil, err
	}
	if dup {
		// A concurrent submission won the key; return its parent and start no steps here.
		return created, nil
	}
	if parent.Status == run.StatusPendingApproval {
		// Held for an approver. Approve starts it, since no claim loop picks up a pipeline parent.
		return parent, nil
	}

	d.wg.Add(1)
	go d.runPipeline(parent.Clone(), steps)

	return parent, nil
}

// runPipeline executes pipeline steps, in order or as a dependency graph, and finalizes the
// parent. The parent registers its own cancel so stopping the parent stops the running steps and
// halts everything that has not started.
func (d *Dispatcher) runPipeline(parent *run.Run, steps []run.PipelineStep) {
	defer d.wg.Done()

	pipeCtx, cancelPipe := context.WithCancel(d.ctx)
	d.register(parent.ID, cancelPipe)
	defer d.unregister(parent.ID)
	defer cancelPipe()

	started := time.Now()
	parent.Status = run.StatusRunning
	parent.StartedAt = &started
	parent.ClaimedBy = d.owner
	parent.ClaimedAt = &started
	d.save(parent)

	watchCtx, stopWatch := context.WithCancel(pipeCtx)
	defer stopWatch()
	go d.watch(watchCtx, parent.ID)

	var failed, canceled bool
	if hasDependencies(steps) {
		failed, canceled = d.runStepsDAG(pipeCtx, parent.Clone(), steps)
	} else {
		failed, canceled = d.runStepsLinear(pipeCtx, parent.Clone(), steps)
	}

	switch {
	case canceled:
		d.finalize(parent, run.StatusCanceled, nil, "")
	case failed:
		code := 1
		d.finalize(parent, run.StatusFailed, &code, "")
	default:
		code := 0
		d.finalize(parent, run.StatusSucceeded, &code, "")
	}
	d.publisher.CloseRun(parent.ID)
}

// runStepsLinear executes the steps one after another, stopping at a failure unless the failing
// step continues on failure. It returns whether any step failed and whether execution was
// canceled.
func (d *Dispatcher) runStepsLinear(ctx context.Context, parent *run.Run, steps []run.PipelineStep) (failed, canceled bool) {
	vars := make(map[string]any)
	for i, step := range steps {
		if ctx.Err() != nil {
			return failed, true
		}

		status, outputs := d.runStepAttempts(ctx, parent, step, i, cloneVars(vars))
		if status == run.StatusCanceled {
			return failed, true
		}
		if status != run.StatusSucceeded {
			failed = true
			if !step.ContinueOnFailure {
				return failed, canceled
			}
			continue
		}
		maps.Copy(vars, outputs)
	}
	return failed, canceled
}

// cloneVars copies a variable map, returning nil for an empty one so runs without inputs stay
// clean.
func cloneVars(vars map[string]any) map[string]any {
	if len(vars) == 0 {
		return nil
	}
	return maps.Clone(vars)
}

// stepRun builds the run a pipeline step executes as. The approval gate and the executor both go
// through this, so a policy is always evaluated against exactly what would run rather than against a
// separately assembled approximation that could drift from it.
func stepRun(parent *run.Run, step run.PipelineStep, idx, attempt int, vars map[string]any) *run.Run {
	inventory := step.Inventory
	if inventory == "" {
		inventory = parent.Inventory
	}
	i := idx
	child := &run.Run{
		ID: run.NewID(), Playbook: step.Playbook, Inventory: inventory,
		Tool: step.Tool, Command: step.Command,
		Status: run.StatusPending, CreatedAt: time.Now(),
		ParentID: &parent.ID, StepIndex: &i, StepName: step.Name, Attempt: attempt,
		ExtraVars: vars,
	}
	// A step names its own tool, command, playbook, and inventory, so those are not inherited. How
	// the run is executed still comes from the pipeline: the environment it runs in, the credentials
	// it may use, the project it reads from, the queue it lands on, and how long it may take.
	child.CredentialIDs = parent.CredentialIDs
	child.ProjectID = parent.ProjectID
	child.InventoryID = parent.InventoryID
	child.Queue = parent.Queue
	child.Timeout = parent.Timeout
	child.Image = parent.Image
	child.PullCredentialID = parent.PullCredentialID
	child.Labels = parent.Labels
	child.Actor = parent.Actor
	// A dry-run pipeline is dry all the way down. A step cannot opt out of a parent that was
	// submitted to make no changes: check mode is a promise about the whole run, and a step running
	// for real underneath it would break that promise silently.
	child.DryRun = step.DryRun || parent.DryRun
	// Notifications stay on the parent. The pipeline is the thing that finished, and copying its
	// targets onto every step would page once per step instead of once per run.
	return child
}

// runStepAttempts executes one pipeline step, re-running it until it succeeds or its retry budget
// is spent. Every attempt is its own child run with an attempt number, so each try keeps a full
// matrix, events, and history. The step receives vars as its extra vars, and on success the
// values it published with set_stats come back for its dependents.
func (d *Dispatcher) runStepAttempts(ctx context.Context, parent *run.Run, step run.PipelineStep,
	idx int, vars map[string]any) (run.Status, map[string]any) {
	status := run.StatusFailed
	for attempt := 0; attempt <= step.Retries; attempt++ {
		if ctx.Err() != nil {
			return run.StatusCanceled, nil
		}
		child := stepRun(parent, step, idx, attempt, vars)
		if err := d.store.Save(context.Background(), child); err != nil {
			d.log.Error("dispatch: save pipeline step: "+err.Error(), zap.String("run_id", parent.ID))
			return run.StatusFailed, nil
		}
		d.wake()
		status = d.waitChildren(ctx, []string{child.ID})[0]
		if status == run.StatusSucceeded {
			return status, d.stepOutputs(child)
		}
		if status == run.StatusCanceled {
			return status, nil
		}
	}
	return status, nil
}

// stepOutputs reads a finished step's published outputs from its events and records them on the
// run. It is best effort; a read failure just means no outputs flow downstream. The outputs are
// recorded on a fresh read of the run, never on the coordinator's pre-claim snapshot, because
// saving that stale snapshot would flip the finished step back to pending and a claim loop would
// execute it a second time.
func (d *Dispatcher) stepOutputs(child *run.Run) map[string]any {
	fold := run.NewSummaryFold(child.CreatedAt)
	var after int64
	for {
		batch, err := d.store.EventsAfter(context.Background(), child.ID, after, summaryPageSize)
		if err != nil {
			d.log.Error("dispatch: read events for outputs: "+err.Error(), zap.String("run_id", child.ID))
			return nil
		}
		if len(batch) == 0 {
			break
		}
		fold.Add(batch)
		after = batch[len(batch)-1].Seq
		if len(batch) < summaryPageSize {
			break
		}
	}
	outputs := fold.Outputs()
	if len(outputs) == 0 {
		return nil
	}
	fresh, err := d.store.Get(context.Background(), child.ID)
	if err != nil {
		d.log.Error("dispatch: read run for outputs: "+err.Error(), zap.String("run_id", child.ID))
		return outputs
	}
	fresh.Outputs = outputs
	d.save(fresh)
	return outputs
}

// DefaultMaxShards caps how many groups a split fans out into when an operator sets no override, so
// one submission cannot spawn thousands of child runs and overwhelm the coordinator's per-child
// polling and the single store writer. The --max-shards flag raises or lowers it; a split is always
// bounded below by the host count regardless.
const DefaultMaxShards = 512

// partition splits hosts into at most shards groups balanced by expected cost. Each host weighs
// its average duration from costs; a host without history weighs the average of the known costs,
// or one when nothing is known, which degrades to balancing by host count. Hosts are placed
// heaviest first into the group with the least total weight, breaking ties by fewer hosts and then
// lower group index so the result is deterministic.
func partition(hosts []string, shards int, costs map[string]float64) [][]string {
	n := min(shards, len(hosts))
	weights := hostWeights(hosts, costs)

	order := make([]string, len(hosts))
	copy(order, hosts)
	sort.SliceStable(order, func(i, j int) bool {
		if weights[order[i]] != weights[order[j]] {
			return weights[order[i]] > weights[order[j]]
		}
		return order[i] < order[j]
	})

	groups := make([][]string, n)
	totals := make([]float64, n)
	for _, host := range order {
		lightest := 0
		for i := 1; i < n; i++ {
			switch {
			case totals[i] < totals[lightest]:
				lightest = i
			case totals[i] == totals[lightest] && len(groups[i]) < len(groups[lightest]):
				lightest = i
			}
		}
		groups[lightest] = append(groups[lightest], host)
		totals[lightest] += weights[host]
	}
	return groups
}

// hostWeights maps each host to its expected cost. Hosts missing from costs get the average known
// cost so they neither dominate nor vanish, and a flat one when no host has a usable cost.
func hostWeights(hosts []string, costs map[string]float64) map[string]float64 {
	known := 0.0
	knownCount := 0
	for _, host := range hosts {
		if c, ok := costs[host]; ok && c > 0 {
			known += c
			knownCount++
		}
	}
	fallback := 1.0
	if knownCount > 0 {
		fallback = known / float64(knownCount)
	}

	out := make(map[string]float64, len(hosts))
	for _, host := range hosts {
		if c, ok := costs[host]; ok && c > 0 {
			out[host] = c
			continue
		}
		out[host] = fallback
	}
	return out
}

// Close stops accepting new work, cancels in-flight runs, and waits for workers to drain.
func (d *Dispatcher) Close() {
	d.cancel()
	d.wg.Wait()
	d.notifyWG.Wait()
}

// executeLeased runs a claimed run on the worker slot the claim loop already holds.
func (d *Dispatcher) executeLeased(base context.Context, r *run.Run) run.Status {
	runCtx, cancel := context.WithCancel(base)
	d.register(r.ID, cancel)
	defer d.unregister(r.ID)
	defer cancel()

	// A run timeout stops a hung tool from holding this worker slot forever. The cause lets the
	// outcome tell a timeout apart from a user cancel. A per-run timeout overrides the dispatcher
	// default, and either bound stays off when zero.
	timeout := d.runTimeout
	if r.Timeout > 0 {
		timeout = time.Duration(r.Timeout) * time.Second
	}
	if timeout > 0 {
		var stop context.CancelFunc
		runCtx, stop = context.WithTimeoutCause(runCtx, timeout, errRunTimeout)
		defer stop()
	}

	return d.execute(runCtx, r)
}

// execute runs r and returns its terminal status. A terraform or opentofu apply that a plan-content
// policy scopes is planned first and its apply proposed for approval; every other run executes in a
// single phase, unchanged. The run carries this process's lease while it executes: a watcher renews
// it and honors cancel requests written to the store by any process.
func (d *Dispatcher) execute(ctx context.Context, r *run.Run) run.Status {
	policies, perr := d.planGatePolicies(ctx, r)
	if perr != nil {
		// The plan-content gate could not be evaluated, so applying now would apply past a gate
		// nobody checked. The run fails with the reason instead.
		d.finalize(r, run.StatusFailed, nil, perr.Error())
		return run.StatusFailed
	}
	if policies != nil {
		return d.executePlanGate(ctx, r, policies)
	}
	return d.executeRun(ctx, r)
}

// executeRun runs r's spec once, streaming output to the store, and finalizes it from the runner
// outcome. It is the single-phase path taken by every run a plan-content policy does not gate.
func (d *Dispatcher) executeRun(ctx context.Context, r *run.Run) run.Status {
	return d.streamSpec(ctx, r, r.DryRun, nil,
		func(res roundhouse.Result, runErr error, mask *masker) run.Status {
			// Write the summaries and any drift while the run is still non-terminal. The store fences
			// auxiliary writes to a terminal run, so finalizing first would reject the run's own final
			// summaries; ordering the writes before finalize lets them land and drops only a
			// reclaimed-but-alive worker's late writes.
			d.summarize(r)
			if res.Drift {
				d.recordPlanDrift(r)
			}
			return d.outcome(ctx, r, res, runErr, mask)
		})
}

// streamSpec runs one execution of r's spec, streaming combined output and structured events to the
// store, then calls finish with the runner outcome to finalize r while the run's temp files and lease
// watcher are still live. dryRun forces the tool's no-change mode regardless of r.DryRun, which the
// plan gate uses to plan before applying, and tee, when non-nil, also receives the combined output so
// the gate can inspect the plan. A setup failure finalizes r as failed, redacting the detail, and
// returns without calling finish. It always closes the run's output stream before returning, and
// returns finish's status on success or StatusFailed on a setup failure.
func (d *Dispatcher) streamSpec(ctx context.Context, r *run.Run, dryRun bool, tee io.Writer,
	finish func(res roundhouse.Result, runErr error, mask *masker) run.Status) run.Status {
	started := time.Now()
	r.Status = run.StatusRunning
	r.StartedAt = &started
	r.ClaimedBy = d.owner
	r.ClaimedAt = &started
	d.save(r)

	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go d.watch(watchCtx, r.ID)

	eventsPath, cleanup, eventsErr := d.eventsFile(r.ID)
	defer cleanup()
	if eventsErr != nil {
		// Capture is off, so this run will finish with an empty matrix and no events however well
		// it goes. Record why on the run, rather than leaving a green run that shows nothing.
		r.Warning = "event capture unavailable, so this run records no events: " + eventsErr.Error()
		d.save(r)
	}

	parent := ""
	if r.ParentID != nil {
		parent = *r.ParentID
	}
	stop := make(chan struct{})
	tailed := make(chan struct{})
	mask := &masker{}
	go func() {
		defer close(tailed)
		d.tailEvents(r.ID, parent, eventsPath, stop, mask)
	}()

	// fail finalizes r as failed and closes its output stream when a setup step cannot complete, so a
	// run that never reached the runner still records why and stops the tailer.
	fail := func(err error) run.Status {
		close(stop)
		<-tailed
		d.finalize(r, run.StatusFailed, nil, mask.redactString(err.Error()))
		d.publisher.CloseRun(r.ID)
		return run.StatusFailed
	}

	// A cancel requested between the claim and the running save is honored here, before any tool
	// starts. The store keeps the cancel flag sticky across saves, so this read observes it.
	if cur, err := d.store.Get(ctx, r.ID); err == nil && cur.CancelRequested {
		close(stop)
		<-tailed
		d.finalize(r, run.StatusCanceled, nil, "")
		d.publisher.CloseRun(r.ID)
		return run.StatusCanceled
	}

	logs := &logSink{store: d.store, id: r.ID, log: d.log, publisher: d.publisher, mask: mask}
	var sink io.Writer = logs
	if tee != nil {
		sink = io.MultiWriter(sink, tee)
	}
	spec := roundhouse.Spec{
		Playbook: r.Playbook, Inventory: r.Inventory, Tool: r.Tool, Command: r.Command,
		DryRun: dryRun, EventsPath: eventsPath, Limit: r.Limit, ExtraVars: r.ExtraVars,
		Image: r.Image,
	}
	if r.Image != "" {
		if err := d.resolvePullCredential(r.PullCredentialID, &spec); err != nil {
			return fail(err)
		}
	}
	d.refreshOnLaunch(ctx, r)
	invCleanup, invSecrets, err := d.materializeInventory(ctx, r, &spec)
	if err != nil {
		return fail(err)
	}
	defer invCleanup()
	mask.set(invSecrets)

	if err := d.resolveProject(r, &spec); err != nil {
		return fail(err)
	}
	d.applyDefaultImage(&spec)

	credCleanup, secrets, err := d.materializeCredentials(ctx, r, &spec)
	if err != nil {
		credCleanup()
		return fail(err)
	}
	defer credCleanup()
	mask.set(append(secrets, invSecrets...))

	res, runErr := d.runner.Run(ctx, spec, sink)
	// The masker holds back the end of each chunk so a secret split across two of them is caught
	// before either half is emitted. The process can write no more, so the withheld tail is released
	// now, while the run is still live: an append to a finalized run is fenced and would be dropped.
	logs.flush()

	close(stop)
	<-tailed

	status := finish(res, runErr, mask)
	d.publisher.CloseRun(r.ID)
	return status
}

// leaseMissLimit is how many consecutive heartbeat failures mean the lease is really gone. A
// single miss can be a transient store error or a first save that has not landed yet, so one
// failure never kills a run.
const leaseMissLimit = 3

// watch renews the executing run's lease and cancels it when another process requests a stop or
// the lease is convincingly lost. It exits when the run's context ends.
func (d *Dispatcher) watch(ctx context.Context, id string) {
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()
	misses := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := d.store.Heartbeat(context.Background(), id, d.owner); err != nil {
			misses++
			if misses < leaseMissLimit {
				continue
			}
			d.log.Warn("dispatch: lease lost: "+err.Error(), zap.String("run_id", id))
			d.Cancel(id)
			return
		}
		misses = 0
		r, err := d.store.Get(context.Background(), id)
		if err != nil {
			continue
		}
		if r.CancelRequested {
			d.Cancel(id)
			return
		}
	}
}

// summarize computes the run's per host and per task summaries from its events and stores them for
// summarize folds the run's events into its per-host, per-task, and facts summaries.
//
// The events are paged rather than loaded whole. A long run can carry hundreds of thousands of them,
// and unmarshaling the list at once cost hundreds of megabytes at the exact moment several runs tend
// to finish together, which is how a small control node ran out of memory. The fold keeps state
// proportional to hosts and tasks, so peak memory is now one page.
func (d *Dispatcher) summarize(r *run.Run) {
	fold := run.NewSummaryFold(r.CreatedAt)
	var after int64
	for {
		batch, err := d.store.EventsAfter(context.Background(), r.ID, after, summaryPageSize)
		if err != nil {
			d.log.Error("dispatch: read events for summary: "+err.Error(), zap.String("run_id", r.ID))
			return
		}
		if len(batch) == 0 {
			break
		}
		fold.Add(batch)
		after = batch[len(batch)-1].Seq
		if len(batch) < summaryPageSize {
			break
		}
	}
	if summaries := fold.HostSummaries(); len(summaries) > 0 {
		if err := withRetries(func() error {
			return d.store.SaveHostSummary(context.Background(), r.ID, summaries)
		}); err != nil {
			d.log.Error("dispatch: save host summary: "+err.Error(), zap.String("run_id", r.ID))
		}
	}
	if facts := fold.HostFacts(); len(facts) > 0 {
		if err := withRetries(func() error {
			return d.store.SaveHostFacts(context.Background(), r.ID, facts)
		}); err != nil {
			d.log.Error("dispatch: save host facts: "+err.Error(), zap.String("run_id", r.ID))
		}
	}
	if tasks := fold.TaskSummaries(); len(tasks) > 0 {
		if err := withRetries(func() error {
			return d.store.SaveTaskSummary(context.Background(), r.ID, tasks)
		}); err != nil {
			d.log.Error("dispatch: save task summary: "+err.Error(), zap.String("run_id", r.ID))
		}
	}
}

// outcome finalizes r from the run result and returns the terminal status. Failure text passes
// through the run's masker so a runner error cannot leak a resolved secret into the stored run.
func (d *Dispatcher) outcome(
	ctx context.Context, r *run.Run, res roundhouse.Result, err error, mask *masker,
) run.Status {
	switch {
	case err != nil && errors.Is(context.Cause(ctx), errRunTimeout):
		d.finalize(r, run.StatusFailed, nil, "run canceled: exceeded its timeout")
		return run.StatusFailed
	case err != nil && ctx.Err() != nil:
		d.finalize(r, run.StatusCanceled, nil, "")
		return run.StatusCanceled
	case err != nil:
		d.finalize(r, run.StatusFailed, nil, mask.redactString(err.Error()))
		return run.StatusFailed
	case res.ExitCode == 0:
		d.finalize(r, run.StatusSucceeded, &res.ExitCode, "")
		return run.StatusSucceeded
	default:
		d.finalize(r, run.StatusFailed, &res.ExitCode, "")
		return run.StatusFailed
	}
}

// register records a cancel func for a run so it can be stopped by id.
func (d *Dispatcher) register(id string, cancel context.CancelFunc) {
	d.cmu.Lock()
	d.cancels[id] = cancel
	d.cmu.Unlock()
}

// unregister drops a run's cancel func once it is no longer cancelable.
func (d *Dispatcher) unregister(id string) {
	d.cmu.Lock()
	delete(d.cancels, id)
	d.cmu.Unlock()
}

// Cancel stops the pending or executing run with the given id, including a parent split and its
// shards. It reports whether a cancelable run was found.
func (d *Dispatcher) Cancel(id string) bool {
	d.cmu.Lock()
	cancel, ok := d.cancels[id]
	d.cmu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// finalize records the terminal status, exit code, failure detail, and end time of r, and sends
// webhook notifications for top-level runs. It refuses to resurrect a run another actor already moved
// to a different terminal state, such as the janitor interrupting an expired lease, so a slow but
// still alive worker that is reclaimed cannot overwrite the interrupt with a success.
func (d *Dispatcher) finalize(r *run.Run, status run.Status, exitCode *int, failure string) {
	if stored, fenced := d.fencedFinalize(r.ID, status); fenced {
		r.Status = stored
		d.log.Warn("dispatch: run already finalized by another actor, not overwriting",
			zap.String("run_id", r.ID), zap.String("stored", string(stored)),
			zap.String("attempted", string(status)))
		return
	}
	ended := time.Now()
	r.Status = status
	r.ExitCode = exitCode
	r.Error = failure
	r.EndedAt = &ended
	d.save(r)
	d.notify(r)
}

// fencedFinalize reports whether a run must not be finalized to status because another actor already
// moved it to a different terminal state. It first tries to claim the terminal transition atomically
// from running, the state every executing run finalizes from; a successful claim means no other actor
// intervened. When the store cannot compare and swap, such as the relay client, or the run was not in
// running, it falls back to reading the current status. It returns the stored status alongside the
// decision so the caller can reflect reality. A legitimate finalize from a non running state, such as
// a rejected run, is never fenced because its stored status already equals the target. When even a
// retried read cannot establish the stored state, the finalize is fenced: skipping the write risks a
// janitor interrupt on a healthy run, but writing blind risks resurrecting a run another actor
// already terminalized, which is the failure the fence exists to stop.
func (d *Dispatcher) fencedFinalize(id string, status run.Status) (run.Status, bool) {
	ctx := context.Background()
	if moved, err := d.store.TransitionStatus(ctx, id, run.StatusRunning, status); err == nil && moved {
		return status, false
	}
	var cur *run.Run
	if err := withRetries(func() error {
		var err error
		cur, err = d.store.Get(ctx, id)
		return err
	}); err != nil {
		d.log.Warn("dispatch: cannot verify run state, skipping finalize: "+err.Error(),
			zap.String("run_id", id))
		return status, true
	}
	if cur.Status.Terminal() && cur.Status != status {
		return cur.Status, true
	}
	return cur.Status, false
}

// save persists r using a background context so terminal state is recorded even during shutdown.
// A failed save retries briefly, since losing a terminal status strands the run as running.
func (d *Dispatcher) save(r *run.Run) {
	if err := withRetries(func() error {
		return d.store.Save(context.Background(), r)
	}); err != nil {
		d.log.Error("dispatch: save run: "+err.Error(), zap.String("run_id", r.ID))
	}
}

// withRetries runs a store write, retrying transient failures with a short backoff. Concurrent
// executors contend on a single writer under SQLite, so one busy moment must not lose state.
func withRetries(f func() error) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if err = f(); err == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * 75 * time.Millisecond)
	}
	return err
}

// eventsFile creates a temp file for the run's structured events and returns its path and a cleanup
// func. On failure it logs and returns an empty path with the error, which disables event capture:
// the run still executes, but it produces no events, so the caller records why on the run.
func (d *Dispatcher) eventsFile(id string) (string, func(), error) {
	f, err := os.CreateTemp("", "switchtender-events-*.ndjson")
	if err != nil {
		d.log.Error("dispatch: create events file: "+err.Error(), zap.String("run_id", id))
		return "", func() {}, err
	}
	path := f.Name()
	_ = f.Close()
	return path, func() { _ = os.Remove(path) }, nil
}

// tailEvents follows the run's event sidecar file, parsing, storing, and publishing complete lines
// as they appear, until stop is closed and a final drain has run. Each poll tick flushes every new
// line as one batch, so a chatty tool costs one store write per tick instead of one per line.
// Events from a child run are also published under its parent so a split or pipeline page streams
// live. The final drain keeps a trailing line missing its newline, since a killed tool can be cut
// off mid-write and what it managed to publish still belongs to the run.
func (d *Dispatcher) tailEvents(id, parent, path string, stop <-chan struct{}, mask *masker) {
	if path == "" {
		<-stop
		return
	}
	f, err := os.Open(path)
	if err != nil {
		d.log.Error("dispatch: open events file: "+err.Error(), zap.String("run_id", id))
		<-stop
		return
	}
	defer func() { _ = f.Close() }()

	reader := bufio.NewReader(f)
	var partial []byte
	drain := func(final bool) {
		var lines [][]byte
		for {
			chunk, err := reader.ReadBytes('\n')
			if len(chunk) > 0 {
				partial = append(partial, chunk...)
				if partial[len(partial)-1] == '\n' {
					lines = append(lines, append([]byte(nil), partial...))
					partial = partial[:0]
				}
			}
			if err != nil {
				break
			}
		}
		if final && len(partial) > 0 {
			lines = append(lines, append([]byte(nil), partial...))
			partial = partial[:0]
		}
		d.flushEventLines(id, parent, lines, mask)
	}

	ticker := time.NewTicker(tailPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			drain(true)
			return
		case <-ticker.C:
			drain(false)
		}
	}
}

// flushEventLines parses a batch of event lines, redacts known secrets from them, stores them in
// one write, and publishes them, echoing child events to the parent topic when the run belongs to
// a split or pipeline. A single damaged line is logged and skipped so the rest of the batch lands.
func (d *Dispatcher) flushEventLines(id, parent string, lines [][]byte, mask *masker) {
	var events []event.Event
	for _, raw := range lines {
		e, ok, err := event.ParseLine(raw)
		if err != nil {
			d.log.Error("dispatch: parse event line: "+err.Error(), zap.String("run_id", id))
			continue
		}
		if !ok {
			continue
		}
		events = append(events, e)
	}
	if len(events) == 0 {
		return
	}
	for i := range events {
		mask.redactEvent(&events[i])
	}
	if err := withRetries(func() error {
		return d.store.AppendEvents(context.Background(), id, events)
	}); err != nil {
		d.log.Error("dispatch: append events: "+err.Error(), zap.String("run_id", id))
	}
	d.publisher.PublishEvents(id, events)
	if parent != "" {
		d.publisher.PublishEvents(parent, events)
	}
}
