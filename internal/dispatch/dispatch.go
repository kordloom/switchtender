// Package dispatch orchestrates run execution: it accepts run requests, schedules them across a
// bounded worker pool, drives status transitions, and streams output into the store.
package dispatch

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/credential"
	"github.com/dcadolph/yardmaster/internal/event"
	"github.com/dcadolph/yardmaster/internal/project"
	"github.com/dcadolph/yardmaster/internal/roundhouse"
	"github.com/dcadolph/yardmaster/internal/run"
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
	// janitorInterval is how often stale leases are swept.
	janitorInterval = 10 * time.Second
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
}

// Option configures a Dispatcher.
type Option func(*config)

// config holds optional Dispatcher settings before construction.
type config struct {
	// workers is the worker pool size.
	workers int
	// publisher receives live output for streaming.
	publisher Publisher
	// owner identifies this process on leases.
	owner string
	// claimInterval is how often the claim loop polls when idle.
	claimInterval time.Duration
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
}

// WithWorkers sets the worker pool size. Values below one fall back to DefaultWorkers.
func WithWorkers(n int) Option {
	return func(c *config) { c.workers = n }
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
	if cfg.publisher == nil {
		cfg.publisher = noopPublisher{}
	}
	if cfg.owner == "" {
		cfg.owner = defaultOwner()
	}
	if cfg.claimInterval <= 0 {
		cfg.claimInterval = DefaultClaimInterval
	}

	lister, _ := runner.(roundhouse.HostLister)
	ctx, cancel := context.WithCancel(context.Background())
	d := &Dispatcher{
		store:         store,
		runner:        runner,
		log:           log,
		sem:           make(chan struct{}, cfg.workers),
		ctx:           ctx,
		cancel:        cancel,
		publisher:     cfg.publisher,
		hostLister:    lister,
		cancels:       make(map[string]context.CancelFunc),
		owner:         cfg.owner,
		claimInterval: cfg.claimInterval,
		credentials:   cfg.credentials,
		sealer:        cfg.sealer,
		projects:      cfg.projects,
		syncer:        cfg.syncer,
		webhooks:      cfg.webhooks,
	}
	d.wg.Add(2)
	go d.claimLoop()
	go d.janitor()
	return d
}

// defaultOwner builds a lease owner name from the host and process so leases are attributable.
func defaultOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "yardmaster"
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
	for {
		select {
		case d.sem <- struct{}{}:
		case <-d.ctx.Done():
			return
		}

		r, err := d.store.Claim(d.ctx, d.owner)
		if err != nil {
			<-d.sem
			if !errors.Is(err, run.ErrNonePending) && d.ctx.Err() == nil {
				d.log.Error("dispatch: claim: " + err.Error())
			}
			select {
			case <-time.After(d.claimInterval):
			case <-d.ctx.Done():
				return
			}
			continue
		}

		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			defer func() { <-d.sem }()
			d.executeLeased(d.ctx, r)
		}()
	}
}

// janitor sweeps stale leases so runs owned by dead processes requeue or resolve. It runs once
// immediately, covering restarts, then on an interval.
func (d *Dispatcher) janitor() {
	defer d.wg.Done()
	sweep := func() {
		n, err := d.store.ReclaimStale(d.ctx, time.Now().Add(-leaseTTL))
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
	if err := d.validateCredentials(ctx, r.CredentialIDs); err != nil {
		return err
	}
	return d.validateProject(ctx, r.ProjectID)
}

// Submit accepts a run for playbook against inventory and returns the created run in pending state.
// Execution proceeds asynchronously; callers observe progress through the store.
func (d *Dispatcher) Submit(ctx context.Context, playbook, inventory string, opts ...run.SubmitOption) (*run.Run, error) {
	if playbook == "" {
		return nil, ErrNoPlaybook
	}

	r := &run.Run{
		ID:        run.NewID(),
		Playbook:  playbook,
		Inventory: inventory,
		Status:    run.StatusPending,
		CreatedAt: time.Now(),
	}
	run.ApplyOptions(r, opts)
	if err := d.validateRun(ctx, r); err != nil {
		return nil, err
	}
	if err := d.store.Save(ctx, r); err != nil {
		return nil, err
	}
	// Execution happens through the claim loop, here or in any worker sharing the store.
	return r, nil
}

// SubmitSplit shards a run across the inventory and returns the parent run in pending state. Each
// shard runs the same playbook limited to its slice of hosts, and the parent rolls up their result.
// Hosts are packed into shards by their average duration in recent runs so each shard carries a
// similar amount of work; hosts without history balance by count. When shards is below two or the
// inventory has fewer than two hosts, it falls back to a single run.
func (d *Dispatcher) SubmitSplit(ctx context.Context, playbook, inventory string, shards int, opts ...run.SubmitOption) (*run.Run, error) {
	if playbook == "" {
		return nil, ErrNoPlaybook
	}
	if shards < 2 {
		return d.Submit(ctx, playbook, inventory, opts...)
	}
	if d.hostLister == nil {
		return nil, ErrNoHostLister
	}

	hosts, err := d.hostLister.Hosts(ctx, inventory)
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

	groups := partition(hosts, shards, costs)
	count := len(groups)
	parent := &run.Run{
		ID: run.NewID(), Playbook: playbook, Inventory: inventory, Kind: run.KindSplit,
		Status: run.StatusPending, CreatedAt: time.Now(), ShardCount: &count,
	}
	run.ApplyOptions(parent, opts)
	if err := d.validateRun(ctx, parent); err != nil {
		return nil, err
	}
	if err := d.store.Save(ctx, parent); err != nil {
		return nil, err
	}

	parentID := parent.ID
	children := make([]*run.Run, 0, count)
	for i, group := range groups {
		idx, shardCount := i, count
		child := &run.Run{
			ID: run.NewID(), Playbook: playbook, Inventory: inventory,
			Status: run.StatusPending, CreatedAt: time.Now(),
			ParentID: &parentID, ShardIndex: &idx, ShardCount: &shardCount,
			Limit: strings.Join(group, ","), CredentialIDs: parent.CredentialIDs,
			ProjectID: parent.ProjectID,
		}
		if err := d.store.Save(ctx, child); err != nil {
			return nil, err
		}
		children = append(children, child)
	}

	d.wg.Add(1)
	go d.coordinate(parent.Clone(), children)

	return parent, nil
}

// RetryFailedShards creates and starts a new split run that re-runs only the failed shards of a
// finished split parent, keeping each failed shard's host group. Shards that succeeded do not run
// again. The new parent links back to the run it retries through RetryOf.
func (d *Dispatcher) RetryFailedShards(ctx context.Context, parentID string) (*run.Run, error) {
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
	for _, s := range shards {
		if s.Status != run.StatusSucceeded {
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
		ShardCount: &count, RetryOf: &parent.ID,
	}
	if err := d.store.Save(ctx, retry); err != nil {
		return nil, err
	}

	retryID := retry.ID
	children := make([]*run.Run, 0, count)
	for i, shard := range failed {
		idx, shardCount := i, count
		child := &run.Run{
			ID: run.NewID(), Playbook: retry.Playbook, Inventory: retry.Inventory,
			Status: run.StatusPending, CreatedAt: time.Now(),
			ParentID: &retryID, ShardIndex: &idx, ShardCount: &shardCount,
			Limit: shard.Limit, CredentialIDs: shard.CredentialIDs,
			ProjectID: shard.ProjectID,
		}
		if err := d.store.Save(ctx, child); err != nil {
			return nil, err
		}
		children = append(children, child)
	}

	d.wg.Add(1)
	go d.coordinate(retry.Clone(), children)

	return retry, nil
}

// coordinate runs each shard through the worker pool and finalizes the parent from their results.
// The parent registers its own cancel so stopping the parent stops every shard.
func (d *Dispatcher) coordinate(parent *run.Run, children []*run.Run) {
	defer d.wg.Done()

	parentCtx, cancelParent := context.WithCancel(d.ctx)
	d.register(parent.ID, cancelParent)
	defer d.unregister(parent.ID)
	defer cancelParent()

	started := time.Now()
	parent.Status = run.StatusRunning
	parent.StartedAt = &started
	d.save(parent)

	statuses := make([]run.Status, len(children))
	var shards sync.WaitGroup
	for i := range children {
		shards.Add(1)
		go func(i int, child *run.Run) {
			defer shards.Done()
			statuses[i] = d.executeManaged(parentCtx, child)
		}(i, children[i].Clone())
	}
	shards.Wait()

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
	case parentCtx.Err() != nil && anyCanceled:
		d.finalize(parent, run.StatusCanceled, nil, "")
	default:
		code := 1
		d.finalize(parent, run.StatusFailed, &code, "")
	}
	d.publisher.CloseRun(parent.ID)
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
		if s.Playbook == "" {
			return nil, ErrNoPlaybook
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
	if err := d.validateRun(ctx, parent); err != nil {
		return nil, err
	}
	if err := d.store.Save(ctx, parent); err != nil {
		return nil, err
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
	d.save(parent)

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

// runStepAttempts executes one pipeline step, re-running it until it succeeds or its retry budget
// is spent. Every attempt is its own child run with an attempt number, so each try keeps a full
// matrix, events, and history. The step receives vars as its extra vars, and on success the
// values it published with set_stats come back for its dependents.
func (d *Dispatcher) runStepAttempts(ctx context.Context, parent *run.Run, step run.PipelineStep,
	idx int, vars map[string]any) (run.Status, map[string]any) {
	inventory := step.Inventory
	if inventory == "" {
		inventory = parent.Inventory
	}

	status := run.StatusFailed
	for attempt := 0; attempt <= step.Retries; attempt++ {
		if ctx.Err() != nil {
			return run.StatusCanceled, nil
		}
		i := idx
		child := &run.Run{
			ID: run.NewID(), Playbook: step.Playbook, Inventory: inventory,
			Status: run.StatusPending, CreatedAt: time.Now(),
			ParentID: &parent.ID, StepIndex: &i, StepName: step.Name, Attempt: attempt,
			ExtraVars: vars, CredentialIDs: parent.CredentialIDs, ProjectID: parent.ProjectID,
		}
		if err := d.store.Save(context.Background(), child); err != nil {
			d.log.Error("dispatch: save pipeline step: "+err.Error(), zap.String("run_id", parent.ID))
			return run.StatusFailed, nil
		}
		status = d.executeManaged(ctx, child)
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
// run. It is best effort; a read failure just means no outputs flow downstream.
func (d *Dispatcher) stepOutputs(child *run.Run) map[string]any {
	events, err := d.store.Events(context.Background(), child.ID)
	if err != nil {
		d.log.Error("dispatch: read events for outputs: "+err.Error(), zap.String("run_id", child.ID))
		return nil
	}
	outputs := run.OutputsFromEvents(events)
	if len(outputs) == 0 {
		return nil
	}
	child.Outputs = outputs
	d.save(child)
	return outputs
}

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
}

// executeManaged registers cancellation for r, acquires a worker slot, executes it, and returns the
// terminal status. The run context derives from base so canceling base (a shutdown or a parent
// cancel) also stops this run.
func (d *Dispatcher) executeManaged(base context.Context, r *run.Run) run.Status {
	runCtx, cancel := context.WithCancel(base)
	d.register(r.ID, cancel)
	defer d.unregister(r.ID)
	defer cancel()

	select {
	case d.sem <- struct{}{}:
	case <-runCtx.Done():
		d.finalize(r, run.StatusCanceled, nil, "")
		return run.StatusCanceled
	}
	defer func() { <-d.sem }()

	return d.execute(runCtx, r)
}

// executeLeased runs a claimed run on the worker slot the claim loop already holds.
func (d *Dispatcher) executeLeased(base context.Context, r *run.Run) run.Status {
	runCtx, cancel := context.WithCancel(base)
	d.register(r.ID, cancel)
	defer d.unregister(r.ID)
	defer cancel()

	return d.execute(runCtx, r)
}

// execute runs the playbook, streaming output to the store, and returns the terminal status. The
// run carries this process's lease while it executes: a watcher renews it and honors cancel
// requests written to the store by any process.
func (d *Dispatcher) execute(ctx context.Context, r *run.Run) run.Status {
	started := time.Now()
	r.Status = run.StatusRunning
	r.StartedAt = &started
	r.ClaimedBy = d.owner
	r.ClaimedAt = &started
	d.save(r)

	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go d.watch(watchCtx, r.ID)

	eventsPath, cleanup := d.eventsFile(r.ID)
	defer cleanup()

	parent := ""
	if r.ParentID != nil {
		parent = *r.ParentID
	}
	stop := make(chan struct{})
	tailed := make(chan struct{})
	go func() {
		defer close(tailed)
		d.tailEvents(r.ID, parent, eventsPath, stop)
	}()

	sink := &logSink{store: d.store, id: r.ID, log: d.log, publisher: d.publisher}
	spec := roundhouse.Spec{
		Playbook: r.Playbook, Inventory: r.Inventory, EventsPath: eventsPath, Limit: r.Limit,
		ExtraVars: r.ExtraVars,
	}
	if err := d.resolveProject(r, &spec); err != nil {
		close(stop)
		<-tailed
		d.finalize(r, run.StatusFailed, nil, err.Error())
		d.publisher.CloseRun(r.ID)
		return run.StatusFailed
	}

	credCleanup, err := d.materializeCredentials(r, &spec)
	if err != nil {
		credCleanup()
		close(stop)
		<-tailed
		d.finalize(r, run.StatusFailed, nil, err.Error())
		d.publisher.CloseRun(r.ID)
		return run.StatusFailed
	}
	defer credCleanup()

	res, err := d.runner.Run(ctx, spec, sink)

	close(stop)
	<-tailed

	status := d.outcome(ctx, r, res, err)
	d.summarize(r)
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
// cross run queries. It is best effort; a failure is logged and does not affect the run result.
func (d *Dispatcher) summarize(r *run.Run) {
	events, err := d.store.Events(context.Background(), r.ID)
	if err != nil {
		d.log.Error("dispatch: read events for summary: "+err.Error(), zap.String("run_id", r.ID))
		return
	}
	if summaries := run.HostSummariesFromStats(events, r.CreatedAt); len(summaries) > 0 {
		if err := withRetries(func() error {
			return d.store.SaveHostSummary(context.Background(), r.ID, summaries)
		}); err != nil {
			d.log.Error("dispatch: save host summary: "+err.Error(), zap.String("run_id", r.ID))
		}
	}
	if tasks := run.TaskSummariesFromEvents(events, r.CreatedAt); len(tasks) > 0 {
		if err := withRetries(func() error {
			return d.store.SaveTaskSummary(context.Background(), r.ID, tasks)
		}); err != nil {
			d.log.Error("dispatch: save task summary: "+err.Error(), zap.String("run_id", r.ID))
		}
	}
}

// outcome finalizes r from the run result and returns the terminal status.
func (d *Dispatcher) outcome(ctx context.Context, r *run.Run, res roundhouse.Result, err error) run.Status {
	switch {
	case err != nil && ctx.Err() != nil:
		d.finalize(r, run.StatusCanceled, nil, "")
		return run.StatusCanceled
	case err != nil:
		d.finalize(r, run.StatusFailed, nil, err.Error())
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
// webhook notifications for top-level runs.
func (d *Dispatcher) finalize(r *run.Run, status run.Status, exitCode *int, failure string) {
	ended := time.Now()
	r.Status = status
	r.ExitCode = exitCode
	r.Error = failure
	r.EndedAt = &ended
	d.save(r)
	d.notify(r)
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

// eventsFile creates a temp file for the run's structured events and returns its path and a
// cleanup func. On failure it logs and returns an empty path, which disables event capture.
func (d *Dispatcher) eventsFile(id string) (string, func()) {
	f, err := os.CreateTemp("", "yardmaster-events-*.ndjson")
	if err != nil {
		d.log.Error("dispatch: create events file: "+err.Error(), zap.String("run_id", id))
		return "", func() {}
	}
	path := f.Name()
	_ = f.Close()
	return path, func() { _ = os.Remove(path) }
}

// tailEvents follows the run's event sidecar file, parsing, storing, and publishing each complete
// line as it appears, until stop is closed and a final drain has run. Events from a child run are
// also published under its parent so a split or pipeline page streams live.
func (d *Dispatcher) tailEvents(id, parent, path string, stop <-chan struct{}) {
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
	drain := func() {
		for {
			chunk, err := reader.ReadBytes('\n')
			if len(chunk) > 0 {
				partial = append(partial, chunk...)
				if partial[len(partial)-1] == '\n' {
					d.handleEventLine(id, parent, partial)
					partial = partial[:0]
				}
			}
			if err != nil {
				return
			}
		}
	}

	ticker := time.NewTicker(tailPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			drain()
			return
		case <-ticker.C:
			drain()
		}
	}
}

// handleEventLine parses a single event line, stores it, and publishes it, echoing child events to
// the parent topic when the run belongs to a split or pipeline.
func (d *Dispatcher) handleEventLine(id, parent string, raw []byte) {
	line := bytes.TrimSpace(raw)
	if len(line) == 0 {
		return
	}
	events, err := event.Parse(bytes.NewReader(line))
	if err != nil {
		d.log.Error("dispatch: parse event line: "+err.Error(), zap.String("run_id", id))
		return
	}
	if len(events) == 0 {
		return
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
