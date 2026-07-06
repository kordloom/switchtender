package schedule

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/run"
)

// DefaultInterval is how often the scheduler checks for due schedules when none is configured.
const DefaultInterval = 15 * time.Second

// Submitter fires a schedule's target. The dispatcher satisfies it.
type Submitter interface {
	// Submit fires a single run.
	Submit(ctx context.Context, playbook, inventory string, opts ...run.SubmitOption) (*run.Run, error)
	// SubmitSplit fires a split run.
	SubmitSplit(ctx context.Context, playbook, inventory string, shards int, opts ...run.SubmitOption) (*run.Run, error)
	// SubmitPipeline fires a pipeline run.
	SubmitPipeline(ctx context.Context, name, inventory string, steps []run.PipelineStep, opts ...run.SubmitOption) (*run.Run, error)
}

// Scheduler fires due schedules on a fixed cadence.
type Scheduler struct {
	// store reads and updates schedules.
	store Store
	// submitter fires a schedule's target.
	submitter Submitter
	// log records scheduler activity.
	log *zap.Logger
	// interval is how often due schedules are checked.
	interval time.Duration
	// ctx is canceled by Close to stop the loop.
	ctx context.Context
	// cancel cancels ctx.
	cancel context.CancelFunc
	// done closes when the loop exits.
	done chan struct{}
}

// SchedulerOption configures a Scheduler.
type SchedulerOption func(*Scheduler)

// WithInterval sets how often due schedules are checked. Values below one are ignored.
func WithInterval(d time.Duration) SchedulerOption {
	return func(s *Scheduler) {
		if d > 0 {
			s.interval = d
		}
	}
}

// NewScheduler returns a Scheduler. It panics if store or submitter is nil; a nil logger is a no-op.
func NewScheduler(store Store, submitter Submitter, log *zap.Logger, opts ...SchedulerOption) *Scheduler {
	if store == nil {
		panic("schedule: Store required")
	}
	if submitter == nil {
		panic("schedule: Submitter required")
	}
	if log == nil {
		log = zap.NewNop()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Scheduler{
		store: store, submitter: submitter, log: log, interval: DefaultInterval,
		ctx: ctx, cancel: cancel, done: make(chan struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Start begins the scheduler loop in a background goroutine.
func (s *Scheduler) Start() {
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case t := <-ticker.C:
				s.tick(t)
			}
		}
	}()
}

// Close stops the scheduler loop and waits for it to exit.
func (s *Scheduler) Close() {
	s.cancel()
	<-s.done
}

// tick fires every schedule due at now and advances its next run time.
func (s *Scheduler) tick(now time.Time) {
	schedules, err := s.store.List(s.ctx)
	if err != nil {
		s.log.Error("schedule: list: " + err.Error())
		return
	}
	for _, sc := range schedules {
		if !sc.Enabled || sc.NextRunAt == nil || sc.NextRunAt.After(now) {
			continue
		}

		runID, err := s.fire(s.ctx, sc)
		if err != nil {
			s.log.Error("schedule: fire: "+err.Error(), zap.String("schedule_id", sc.ID))
		}

		next, err := NextFire(sc.Cron, now)
		if err != nil {
			s.log.Error("schedule: next fire: "+err.Error(), zap.String("schedule_id", sc.ID))
			continue
		}
		sc.LastRunAt = &now
		if runID != "" {
			sc.LastRunID = runID
		}
		sc.NextRunAt = &next
		if err := s.store.Save(s.ctx, sc); err != nil {
			s.log.Error("schedule: save: "+err.Error(), zap.String("schedule_id", sc.ID))
		}
	}
}

// fire submits the schedule's target and returns the created run id.
func (s *Scheduler) fire(ctx context.Context, sc *Schedule) (string, error) {
	var (
		created *run.Run
		err     error
	)
	switch {
	case len(sc.Steps) > 0:
		created, err = s.submitter.SubmitPipeline(ctx, sc.Name, sc.Inventory, sc.Steps)
	case sc.Shards >= 2:
		created, err = s.submitter.SubmitSplit(ctx, sc.Playbook, sc.Inventory, sc.Shards)
	default:
		created, err = s.submitter.Submit(ctx, sc.Playbook, sc.Inventory)
	}
	if err != nil {
		return "", err
	}
	s.log.Info("schedule fired",
		zap.String("schedule_id", sc.ID), zap.String("run_id", created.ID))
	return created.ID, nil
}
