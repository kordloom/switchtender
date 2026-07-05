// Package dispatch orchestrates run execution: it accepts run requests, schedules them across a
// bounded worker pool, drives status transitions, and streams output into the store.
package dispatch

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/event"
	"github.com/dcadolph/yardmaster/internal/roundhouse"
	"github.com/dcadolph/yardmaster/internal/run"
)

// DefaultWorkers is the number of concurrent runs when none is configured.
const DefaultWorkers = 4

// tailPollInterval is how often the event tailer checks the sidecar file for new lines.
const tailPollInterval = 75 * time.Millisecond

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
}

// Option configures a Dispatcher.
type Option func(*config)

// config holds optional Dispatcher settings before construction.
type config struct {
	// workers is the worker pool size.
	workers int
	// publisher receives live output for streaming.
	publisher Publisher
}

// WithWorkers sets the worker pool size. Values below one fall back to DefaultWorkers.
func WithWorkers(n int) Option {
	return func(c *config) { c.workers = n }
}

// WithPublisher sets the Publisher that receives live events and log chunks.
func WithPublisher(p Publisher) Option {
	return func(c *config) { c.publisher = p }
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

	lister, _ := runner.(roundhouse.HostLister)
	ctx, cancel := context.WithCancel(context.Background())
	return &Dispatcher{
		store:      store,
		runner:     runner,
		log:        log,
		sem:        make(chan struct{}, cfg.workers),
		ctx:        ctx,
		cancel:     cancel,
		publisher:  cfg.publisher,
		hostLister: lister,
		cancels:    make(map[string]context.CancelFunc),
	}
}

// Submit accepts a run for playbook against inventory and returns the created run in pending state.
// Execution proceeds asynchronously; callers observe progress through the store.
func (d *Dispatcher) Submit(ctx context.Context, playbook, inventory string) (*run.Run, error) {
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
	if err := d.store.Save(ctx, r); err != nil {
		return nil, err
	}

	d.wg.Add(1)
	go d.work(r.Clone())

	return r, nil
}

// SubmitSplit shards a run across the inventory and returns the parent run in pending state. Each
// shard runs the same playbook limited to its slice of hosts, and the parent rolls up their result.
// When shards is below two or the inventory has fewer than two hosts, it falls back to a single run.
func (d *Dispatcher) SubmitSplit(ctx context.Context, playbook, inventory string, shards int) (*run.Run, error) {
	if playbook == "" {
		return nil, ErrNoPlaybook
	}
	if shards < 2 {
		return d.Submit(ctx, playbook, inventory)
	}
	if d.hostLister == nil {
		return nil, ErrNoHostLister
	}

	hosts, err := d.hostLister.Hosts(ctx, inventory)
	if err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}
	if len(hosts) < 2 {
		return d.Submit(ctx, playbook, inventory)
	}

	groups := partition(hosts, shards)
	count := len(groups)
	parent := &run.Run{
		ID: run.NewID(), Playbook: playbook, Inventory: inventory,
		Status: run.StatusPending, CreatedAt: time.Now(), ShardCount: &count,
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
			Limit: strings.Join(group, ","),
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
}

// partition splits hosts into at most shards groups balanced by host count using round robin.
func partition(hosts []string, shards int) [][]string {
	n := shards
	if n > len(hosts) {
		n = len(hosts)
	}
	groups := make([][]string, n)
	for i, host := range hosts {
		groups[i%n] = append(groups[i%n], host)
	}
	return groups
}

// Close stops accepting new work, cancels in-flight runs, and waits for workers to drain.
func (d *Dispatcher) Close() {
	d.cancel()
	d.wg.Wait()
}

// Reconcile marks runs left non-terminal by a previous process as interrupted, since their owning
// process is gone and they cannot resume. It returns the number reconciled and is meant to run once
// at startup before serving.
func (d *Dispatcher) Reconcile(ctx context.Context) (int, error) {
	runs, err := d.store.NonTerminal(ctx)
	if err != nil {
		return 0, err
	}
	for _, r := range runs {
		ended := time.Now()
		r.Status = run.StatusInterrupted
		r.EndedAt = &ended
		if r.Error == "" {
			r.Error = "interrupted: server restarted"
		}
		if err := d.store.Save(ctx, r); err != nil {
			return 0, err
		}
	}
	return len(runs), nil
}

// work runs a single submitted run through the pool.
func (d *Dispatcher) work(r *run.Run) {
	defer d.wg.Done()
	d.executeManaged(d.ctx, r)
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

// execute runs the playbook, streaming output to the store, and returns the terminal status.
func (d *Dispatcher) execute(ctx context.Context, r *run.Run) run.Status {
	started := time.Now()
	r.Status = run.StatusRunning
	r.StartedAt = &started
	d.save(r)

	eventsPath, cleanup := d.eventsFile(r.ID)
	defer cleanup()

	stop := make(chan struct{})
	tailed := make(chan struct{})
	go func() {
		defer close(tailed)
		d.tailEvents(r.ID, eventsPath, stop)
	}()

	sink := &logSink{store: d.store, id: r.ID, log: d.log, publisher: d.publisher}
	spec := roundhouse.Spec{
		Playbook: r.Playbook, Inventory: r.Inventory, EventsPath: eventsPath, Limit: r.Limit,
	}
	res, err := d.runner.Run(ctx, spec, sink)

	close(stop)
	<-tailed

	status := d.outcome(ctx, r, res, err)
	d.summarize(r)
	d.publisher.CloseRun(r.ID)
	return status
}

// summarize computes the run's per host outcome summary from its events and stores it for cross run
// queries. It is best effort; a failure is logged and does not affect the run result.
func (d *Dispatcher) summarize(r *run.Run) {
	events, err := d.store.Events(context.Background(), r.ID)
	if err != nil {
		d.log.Error("dispatch: read events for summary: "+err.Error(), zap.String("run_id", r.ID))
		return
	}
	summaries := run.HostSummariesFromStats(events, r.CreatedAt)
	if len(summaries) == 0 {
		return
	}
	if err := d.store.SaveHostSummary(context.Background(), r.ID, summaries); err != nil {
		d.log.Error("dispatch: save host summary: "+err.Error(), zap.String("run_id", r.ID))
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

// finalize records the terminal status, exit code, failure detail, and end time of r.
func (d *Dispatcher) finalize(r *run.Run, status run.Status, exitCode *int, failure string) {
	ended := time.Now()
	r.Status = status
	r.ExitCode = exitCode
	r.Error = failure
	r.EndedAt = &ended
	d.save(r)
}

// save persists r using a background context so terminal state is recorded even during shutdown.
func (d *Dispatcher) save(r *run.Run) {
	if err := d.store.Save(context.Background(), r); err != nil {
		d.log.Error("dispatch: save run: "+err.Error(), zap.String("run_id", r.ID))
	}
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
// line as it appears, until stop is closed and a final drain has run.
func (d *Dispatcher) tailEvents(id, path string, stop <-chan struct{}) {
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
					d.handleEventLine(id, partial)
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

// handleEventLine parses a single event line, stores it, and publishes it.
func (d *Dispatcher) handleEventLine(id string, raw []byte) {
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
	if err := d.store.AppendEvents(context.Background(), id, events); err != nil {
		d.log.Error("dispatch: append events: "+err.Error(), zap.String("run_id", id))
	}
	d.publisher.PublishEvents(id, events)
}
