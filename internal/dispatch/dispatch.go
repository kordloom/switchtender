// Package dispatch orchestrates run execution: it accepts run requests, schedules them across a
// bounded worker pool, drives status transitions, and streams output into the store.
package dispatch

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/roundhouse"
	"github.com/dcadolph/yardmaster/internal/run"
)

// DefaultWorkers is the number of concurrent runs when none is configured.
const DefaultWorkers = 4

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
}

// Option configures a Dispatcher.
type Option func(*config)

// config holds optional Dispatcher settings before construction.
type config struct {
	// workers is the worker pool size.
	workers int
}

// WithWorkers sets the worker pool size. Values below one fall back to DefaultWorkers.
func WithWorkers(n int) Option {
	return func(c *config) { c.workers = n }
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

	ctx, cancel := context.WithCancel(context.Background())
	return &Dispatcher{
		store:  store,
		runner: runner,
		log:    log,
		sem:    make(chan struct{}, cfg.workers),
		ctx:    ctx,
		cancel: cancel,
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

	sink := &logSink{store: d.store, id: r.ID, log: d.log}
	spec := roundhouse.Spec{Playbook: r.Playbook, Inventory: r.Inventory}
	res, err := d.runner.Run(d.ctx, spec, sink)

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
