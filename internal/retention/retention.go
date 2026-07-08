// Package retention trims a run store on a schedule so a busy fleet's database does not grow
// forever. It drops the events and logs of old runs, then deletes runs older than a longer window,
// always keeping the per host and per task summaries that power the cross-run views.
package retention

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/run"
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
	return s.retainRuns > 0 || s.retainEvents > 0
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

// sweep trims events older than the events window, then deletes runs older than the runs window.
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
}

// Close stops the sweep loop and waits for it to finish.
func (s *Sweeper) Close() {
	s.cancel()
	s.wg.Wait()
}
