// Package spanbeat appends a span beat to the audit chain on a fixed cadence, so an outside
// watcher can tell a quiet chain from one whose tail was removed. Each beat numbers itself one
// past the last and counts the entries appended since, and a bundle over a chain with a duplicate
// or missing beat fails verification, which is what makes the silence itself attestable.
package spanbeat

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
)

// AnchorFunc fixes a freshly appended beat entry somewhere outside this install. The wiring
// decides what an anchor is, so this package stays free of network code.
type AnchorFunc func(ctx context.Context, e *audit.Entry) error

// Emitter periodically appends a span beat to the audit chain.
type Emitter struct {
	// store is the audit store the beats are appended to.
	store audit.Store
	// cadence is how often a beat is appended. It is recorded in every beat entry, which is why it
	// must be a whole number of seconds.
	cadence time.Duration
	// anchor, when set, fixes each successful beat outside this install. It is only called when
	// the store also keeps anchors, since an anchor that cannot be saved proves nothing.
	anchor AnchorFunc
	// log records beat activity.
	log *zap.Logger
	// ctx is canceled by Close to stop the beat loop.
	ctx context.Context
	// cancel cancels ctx.
	cancel context.CancelFunc
	// wg tracks the beat goroutine so Close can wait for it.
	wg sync.WaitGroup
}

// Option configures an Emitter.
type Option func(*Emitter)

// WithAnchorFunc sets the function that anchors each successfully appended beat.
func WithAnchorFunc(f AnchorFunc) Option {
	return func(e *Emitter) { e.anchor = f }
}

// NewEmitter returns an Emitter for the store. It panics on a nil store or on a cadence that is
// not a whole number of seconds of at least one, since the cadence is committed into every beat
// entry; a nil logger becomes a no-op.
func NewEmitter(store audit.Store, cadence time.Duration, log *zap.Logger, opts ...Option) *Emitter {
	if store == nil {
		panic("spanbeat: Store required")
	}
	if cadence < time.Second || cadence%time.Second != 0 {
		panic("spanbeat: cadence must be a whole number of seconds, at least one")
	}
	if log == nil {
		log = zap.NewNop()
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Emitter{store: store, cadence: cadence, log: log, ctx: ctx, cancel: cancel}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Start launches the beat loop, which appends one beat immediately and then one per cadence, until
// Close. The immediate beat marks the restart itself, so a watcher sees the process come back
// rather than an unexplained gap.
func (e *Emitter) Start() {
	e.wg.Go(func() {
		e.beat(time.Now())
		ticker := time.NewTicker(e.cadence)
		defer ticker.Stop()
		for {
			select {
			case <-e.ctx.Done():
				return
			case now := <-ticker.C:
				e.beat(now)
			}
		}
	})
}

// beat appends one span beat, then anchors it when an anchor function is configured and the store
// keeps anchors. A failure of either is logged and the loop carries on: a missed beat shows up in
// the feed as a widening gap, which is exactly the signal the beats exist to give.
func (e *Emitter) beat(now time.Time) {
	entry, err := e.store.AppendSpanBeat(e.ctx, now, int(e.cadence/time.Second))
	if err != nil {
		e.log.Error("spanbeat: append beat: " + err.Error())
		return
	}
	beat, _, _, _ := audit.ParseSpanPath(entry.Path)
	e.log.Debug("spanbeat: beat appended", zap.Int64("beat", beat), zap.Int64("seq", entry.Seq))
	if e.anchor == nil {
		return
	}
	if _, ok := e.store.(audit.AnchorStore); !ok {
		return
	}
	if err := e.anchor(e.ctx, entry); err != nil {
		e.log.Error("spanbeat: anchor beat: " + err.Error())
	}
}

// Close stops the beat loop and waits for it to finish.
func (e *Emitter) Close() {
	e.cancel()
	e.wg.Wait()
}
