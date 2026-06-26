// Package dispatch orchestrates run execution: it accepts run requests, schedules them across a
// bounded worker pool, drives status transitions, and streams output into the store.
package dispatch

import (
	"bufio"
	"bytes"
	"context"
	"os"
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

	ctx, cancel := context.WithCancel(context.Background())
	return &Dispatcher{
		store:     store,
		runner:    runner,
		log:       log,
		sem:       make(chan struct{}, cfg.workers),
		ctx:       ctx,
		cancel:    cancel,
		publisher: cfg.publisher,
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

// Close stops accepting new work, cancels in-flight runs, and waits for workers to drain.
func (d *Dispatcher) Close() {
	d.cancel()
	d.wg.Wait()
}

// work acquires a worker slot then executes r, marking it canceled if shutdown wins the race.
func (d *Dispatcher) work(r *run.Run) {
	defer d.wg.Done()

	select {
	case d.sem <- struct{}{}:
	case <-d.ctx.Done():
		d.finalize(r, run.StatusCanceled, nil, "")
		return
	}
	defer func() { <-d.sem }()

	d.execute(r)
}

// execute runs the playbook, streaming output to the store and recording the terminal state.
func (d *Dispatcher) execute(r *run.Run) {
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
	spec := roundhouse.Spec{Playbook: r.Playbook, Inventory: r.Inventory, EventsPath: eventsPath}
	res, err := d.runner.Run(d.ctx, spec, sink)

	close(stop)
	<-tailed

	switch {
	case err != nil && d.ctx.Err() != nil:
		d.finalize(r, run.StatusCanceled, nil, "")
	case err != nil:
		d.finalize(r, run.StatusFailed, nil, err.Error())
	case res.ExitCode == 0:
		d.finalize(r, run.StatusSucceeded, &res.ExitCode, "")
	default:
		d.finalize(r, run.StatusFailed, &res.ExitCode, "")
	}

	d.publisher.CloseRun(r.ID)
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
