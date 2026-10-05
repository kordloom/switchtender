package attention

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// DefaultMonitorInterval is how often the monitor looks for work past its alert threshold. The
// shortest default threshold is two lease periods, a minute, so a check every fifteen seconds
// raises an alert within a quarter of the shortest wait it measures.
const DefaultMonitorInterval = 15 * time.Second

// Alerter delivers an attention alert. The dispatcher satisfies it, and delivers the alert through
// the same channels and named targets a held run reaches, as the alert's own event.
type Alerter interface {
	// NotifyAttention delivers r, which carries the alert in its Attention field.
	NotifyAttention(r *run.Run)
}

// AlerterFunc adapts a function to an Alerter.
type AlerterFunc func(r *run.Run)

// NotifyAttention calls f.
func (f AlerterFunc) NotifyAttention(r *run.Run) { f(r) }

// Monitor raises an alert, once, for each item that has been in its main blocker past its alert
// threshold. Every server runs one. The store records each alert the first time any of them
// raises it, so replicas sharing a database alert once between them.
type Monitor struct {
	// source reads what needs attention.
	source *Source
	// store records which alerts were raised.
	store Store
	// alerter delivers an alert.
	alerter Alerter
	// log records a sweep that could not run, never an alert's content.
	log *zap.Logger
	// interval is how often the monitor sweeps.
	interval time.Duration
	// ctx is canceled by Close.
	ctx context.Context
	// cancel cancels ctx.
	cancel context.CancelFunc
	// wg tracks the sweep loop.
	wg sync.WaitGroup
	// startOnce launches the loop at most once.
	startOnce sync.Once
}

// NewMonitor returns a Monitor. It panics on a nil source, store, or alerter, which is a wiring
// mistake. A nil logger is a no-op and an interval of zero or less uses DefaultMonitorInterval.
func NewMonitor(source *Source, store Store, alerter Alerter, log *zap.Logger,
	interval time.Duration) *Monitor {
	if source == nil {
		panic("attention: Source required")
	}
	if store == nil {
		panic("attention: Store required")
	}
	if alerter == nil {
		panic("attention: Alerter required")
	}
	if log == nil {
		log = zap.NewNop()
	}
	if interval <= 0 {
		interval = DefaultMonitorInterval
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Monitor{source: source, store: store, alerter: alerter, log: log, interval: interval,
		ctx: ctx, cancel: cancel}
}

// Start begins sweeping on the interval in a background goroutine. A second call does nothing.
func (m *Monitor) Start() {
	m.startOnce.Do(func() {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			ticker := time.NewTicker(m.interval)
			defer ticker.Stop()
			for {
				select {
				case <-m.ctx.Done():
					return
				case <-ticker.C:
					if _, err := m.Sweep(m.ctx); err != nil && m.ctx.Err() == nil {
						m.log.Error("attention: sweep: " + err.Error())
					}
				}
			}
		}()
	})
}

// Close stops the sweep loop and waits for it to exit.
func (m *Monitor) Close() {
	m.cancel()
	m.wg.Wait()
}

// Sweep evaluates once and raises every alert that is due and not yet raised, returning how many
// it raised. An item whose alert another server already raised is skipped, and so is one this
// store cannot record, since an alert it cannot record would be raised again on every sweep.
func (m *Monitor) Sweep(ctx context.Context) (int, error) {
	snap, err := m.source.Snapshot(ctx)
	if err != nil {
		return 0, err
	}
	raised := 0
	for _, it := range snap.Items {
		if !it.Alerting || it.Run == nil {
			continue
		}
		won, cerr := m.store.ClaimAlert(ctx, it.AlertKey)
		if cerr != nil {
			m.log.Error("attention: record an alert: "+cerr.Error(), zap.String("item", it.Key))
			continue
		}
		if !won {
			continue
		}
		alert := it.Run.Clone()
		alert.Attention = it.Note(snap.GeneratedAt)
		m.alerter.NotifyAttention(alert)
		raised++
	}
	return raised, nil
}
