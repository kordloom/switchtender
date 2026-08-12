// Package retention trims a run store on a schedule so a busy fleet's database does not grow
// forever. It drops the events and logs of old runs, then deletes runs older than a longer window,
// keeping the per host and per task summaries that power the cross-run views. Those summaries
// outlive their runs on purpose, so they are bounded by count rather than by age: a sweep keeps the
// newest few hundred per host and per task and drops the rest.
package retention

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// DefaultInterval is how often the sweeper runs when none is configured.
const DefaultInterval = time.Hour

// Sweeper periodically purges old events and runs from the store.
type Sweeper struct {
	// store is the run store to trim.
	store run.Store
	// retainRuns is how long a terminal run is kept before deletion. Zero disables run deletion.
	retainRuns time.Duration
	// retainEvents is how long a run's events and logs are kept. Zero disables event trimming.
	retainEvents time.Duration
	// retainHistory is how many summaries per host and per task survive a sweep. Zero disables
	// history trimming, which leaves the two summary tables growing forever.
	retainHistory int
	// interval is how often the sweeper runs.
	interval time.Duration
	// log records sweep activity.
	log *zap.Logger
	// ctx is canceled by Close to stop the sweep loop.
	ctx context.Context
	// cancel cancels ctx.
	cancel context.CancelFunc
	// wg tracks the sweep goroutine so Close can wait for it.
	wg sync.WaitGroup
}

// Option configures a Sweeper.
type Option func(*Sweeper)

// WithRetainRuns sets how long terminal runs are kept before deletion. Zero disables it.
func WithRetainRuns(d time.Duration) Option {
	return func(s *Sweeper) { s.retainRuns = d }
}

// WithRetainEvents sets how long a run's events and logs are kept. Zero disables it.
func WithRetainEvents(d time.Duration) Option {
	return func(s *Sweeper) { s.retainEvents = d }
}

// WithRetainHistory sets how many summaries per host and per task survive a sweep. Zero disables
// history trimming. A positive count below run.MinRetainSummaries is raised to it, because the
// fleet views let a caller ask for a window that deep and answering a legal window with a truncated
// history would report a host as healthier, or quieter, than it was.
func WithRetainHistory(n int) Option {
	return func(s *Sweeper) {
		if n > 0 && n < run.MinRetainSummaries {
			n = run.MinRetainSummaries
		}
		s.retainHistory = n
	}
}

// WithInterval sets how often the sweeper runs.
func WithInterval(d time.Duration) Option {
	return func(s *Sweeper) { s.interval = d }
}

// NewSweeper returns a Sweeper for the store. It panics if store is nil; a nil logger becomes a
// no-op.
func NewSweeper(store run.Store, log *zap.Logger, opts ...Option) *Sweeper {
	if store == nil {
		panic("retention: Store required")
	}
	if log == nil {
		log = zap.NewNop()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Sweeper{store: store, interval: DefaultInterval, log: log, ctx: ctx, cancel: cancel}
	for _, opt := range opts {
		opt(s)
	}
	if s.interval <= 0 {
		s.interval = DefaultInterval
	}
	return s
}

// Enabled reports whether any retention window is configured, so a caller can skip starting a
// sweeper that would do nothing.
func (s *Sweeper) Enabled() bool {
	return s.retainRuns > 0 || s.retainEvents > 0 || s.retainHistory > 0
}

// Start launches the sweep loop, which runs once immediately and then on the interval, until Close.
// It does nothing when no retention window is configured.
func (s *Sweeper) Start() {
	if !s.Enabled() {
		return
	}
	s.wg.Go(func() {
		s.sweep(time.Now())
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case now := <-ticker.C:
				s.sweep(now)
			}
		}
	})
}

// sweep trims events older than the events window, deletes runs older than the runs window, then
// bounds the summary tables. Trimming summaries comes last because deleting runs does not touch
// them: they are kept on purpose so a host's outcome history outlives the runs it came from, which
// is also why they are the only tables with no other bound.
func (s *Sweeper) sweep(now time.Time) {
	if s.retainEvents > 0 {
		n, err := s.store.PurgeEventsBefore(s.ctx, now.Add(-s.retainEvents))
		if err != nil {
			s.log.Error("retention: purge events: " + err.Error())
		} else if n > 0 {
			s.log.Info("retention: trimmed run events", zap.Int("runs", n))
		}
	}
	if s.retainRuns > 0 {
		n, err := s.store.PurgeRunsBefore(s.ctx, now.Add(-s.retainRuns))
		if err != nil {
			s.log.Error("retention: purge runs: " + err.Error())
		} else if n > 0 {
			s.log.Info("retention: deleted old runs", zap.Int("runs", n))
		}
	}
	if s.retainHistory > 0 {
		n, err := s.store.TrimSummaries(s.ctx, s.retainHistory)
		if err != nil {
			s.log.Error("retention: trim summaries: " + err.Error())
		} else if n > 0 {
			s.log.Info("retention: trimmed host and task summaries", zap.Int("rows", n))
		}
	}
}

// Close stops the sweep loop and waits for it to finish.
func (s *Sweeper) Close() {
	s.cancel()
	s.wg.Wait()
}
