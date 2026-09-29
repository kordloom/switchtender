// Package spanbeat appends a span beat to an audit chain on a fixed cadence, so an outside watcher
// can tell a quiet chain from one whose tail was removed. Each beat numbers itself one past the last
// and counts the entries appended since, and a bundle over a chain with a duplicate or missing beat
// fails verification, which is what makes the silence itself attestable.
//
// A beat whose time does not advance past the last one is not written. A beat's time is a signed
// claim, so recording a time the clock did not read would be a false statement in an attestation.
// The emitter logs the refusal and keeps beating, and the record carries a longer unattested window
// that a verifier reports as a gap.
//
// The emitter's only coupling to a chain is the Store interface, so it imports nothing from the
// product and an out-of-tree tool embeds it by adapting its own chain to Store.
package spanbeat

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// AppendedBeat is what a Store reports after writing one beat: enough for the emitter to log the
// beat, mark when the population was last counted, and anchor the entry outside the install.
type AppendedBeat struct {
	// At is when the beat was appended.
	At time.Time
	// Seq is the beat entry's position in the chain.
	Seq int64
	// Hash is the beat entry's own chain hash, the head the beat attests.
	Hash string
	// Beat is the beat number, one past the last.
	Beat int64
}

// Store appends one span beat and reports what it wrote. It is the emitter's only coupling to a
// chain, so any audit backend satisfies it through a small adapter and nothing here imports the
// chain.
//
// A store whose clock has not passed the last beat must return an error whose chain carries a value
// with a ClockBehind method. The emitter then logs the gap and keeps its cadence rather than
// treating it as a hard failure, because a beat's time is a signed claim and a time the clock did
// not read would be a false statement in an attestation.
type Store interface {
	// AppendSpanBeat writes one beat recorded at the given time, carrying the cadence in whole
	// seconds, and returns what it wrote.
	AppendSpanBeat(ctx context.Context, at time.Time, cadenceSeconds int) (AppendedBeat, error)
}

// clockBehind is satisfied by an append error meaning the clock has not passed the last beat. The
// emitter detects it structurally, so a store reports the condition without the emitter importing
// the store's error type.
type clockBehind interface {
	// ClockBehind reports the refused beat number, the last beat's recorded time, and the time the
	// clock read.
	ClockBehind() (beat int64, last, clock time.Time)
}

// Stats is a reading of the process-wide beat counters.
type Stats struct {
	// Last is when this process last appended a beat, zero when it has appended none.
	Last time.Time
	// Appended is how many beats this process has written since start.
	Appended int64
	// Suppressed is how many beats were refused because the clock had not passed the last beat.
	// Each one is an unattested window that the record will show as a gap.
	Suppressed int64
}

// Beat counters live in the package rather than on an Emitter because the metrics endpoint is built
// from the run store and cannot reach the emitter the serve command created. One process runs one
// emitter, so a package counter and an emitter field would hold the same number either way.
var (
	// appended counts beats written since this process started.
	appended atomic.Int64
	// suppressed counts beats refused because the clock had not passed the last beat.
	suppressed atomic.Int64
	// lastBeatUnixNano holds when the last beat was written, zero when none has been.
	lastBeatUnixNano atomic.Int64
)

// ReadStats returns the beat counters for this process, for the metrics endpoint. Going quiet is
// only a bounded gap in the record, so it has to be loud here: alert on suppressed beats rising or
// on the age of the last beat passing the cadence.
func ReadStats() Stats {
	var last time.Time
	if ns := lastBeatUnixNano.Load(); ns != 0 {
		last = time.Unix(0, ns).UTC()
	}
	return Stats{Last: last, Appended: appended.Load(), Suppressed: suppressed.Load()}
}

// AnchorFunc fixes a freshly appended beat somewhere outside this install. The wiring decides what
// an anchor is, so this package stays free of network code. It receives the appended beat, whose Seq
// and Hash are what an anchor commits to. Configure one only when the store can save what it
// produces, since an anchor that cannot be stored proves nothing.
type AnchorFunc func(ctx context.Context, b AppendedBeat) error

// Emitter periodically appends a span beat to the audit chain. A tick whose clock has not passed
// the last beat writes nothing and logs a warning, since a beat's time is a signed claim and a time
// the clock did not read would be a false statement in an attestation. The skipped beat leaves a
// gap in the record, which a verifier reports rather than fails, and the cadence resumes on its own
// once real time passes the last beat.
type Emitter struct {
	// store is the chain the beats are appended to.
	store Store
	// cadence is how often a beat is appended. It is recorded in every beat entry, which is why it
	// must be a whole number of seconds.
	cadence time.Duration
	// anchor, when set, fixes each successful beat outside this install.
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
func NewEmitter(store Store, cadence time.Duration, log *zap.Logger, opts ...Option) *Emitter {
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

// beat appends one span beat, then anchors it when an anchor function is configured. A failure of
// either is logged and the loop carries on: a missed beat shows up in the feed as a widening gap,
// which is exactly the signal the beats exist to give.
//
// A clock that has not passed the last beat is not a failure to retry differently. The store refuses
// the beat, because a beat's time is a signed claim and writing a time the clock did not read would
// be a false statement in an attestation. The refusal is logged at warning level, the loop keeps its
// cadence, and the beat lands on a later tick once real time passes the last beat. What the record
// shows in the meantime is a gap, which a verifier reports with its bounds and duration rather than
// failing.
func (e *Emitter) beat(now time.Time) {
	b, err := e.store.AppendSpanBeat(e.ctx, now, int(e.cadence/time.Second))
	if err != nil {
		var behind clockBehind
		if errors.As(err, &behind) {
			beat, last, clock := behind.ClockBehind()
			suppressed.Add(1)
			e.log.Warn("spanbeat: this clock has not passed the last beat, so no beat was written "+
				"and the record will show an unattested window until it does. Fix the clock, and "+
				"prefer NTP slewing over stepping",
				zap.Int64("beat", beat), zap.Duration("behind", last.Sub(clock)),
				zap.Time("last_beat", last), zap.Time("clock", clock))
			return
		}
		e.log.Error("spanbeat: append beat: " + err.Error())
		return
	}
	appended.Add(1)
	lastBeatUnixNano.Store(b.At.UnixNano())
	e.log.Debug("spanbeat: beat appended", zap.Int64("beat", b.Beat), zap.Int64("seq", b.Seq))
	if e.anchor == nil {
		return
	}
	if err := e.anchor(e.ctx, b); err != nil {
		e.log.Error("spanbeat: anchor beat: " + err.Error())
	}
}

// Close stops the beat loop and waits for it to finish.
func (e *Emitter) Close() {
	e.cancel()
	e.wg.Wait()
}
