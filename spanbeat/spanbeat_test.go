package spanbeat_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/kordloom/switchtender/spanbeat"
)

// waitFor polls until check passes or the deadline lapses, so timing-driven loops are observed
// through their effects rather than slept past.
func waitFor(t *testing.T, timeout time.Duration, check func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return check()
}

// clockBehindErr is the error a store signals when its clock has not passed the last beat. Its
// ClockBehind method is the whole contract the emitter detects, so a store reports the condition
// without the emitter naming the store's error type. A third party's store returns the same shape.
type clockBehindErr struct {
	beat        int64
	last, clock time.Time
}

// Error names the condition.
func (e *clockBehindErr) Error() string { return "clock has not passed the last beat" }

// ClockBehind reports the refused beat number and the two times it was decided from.
func (e *clockBehindErr) ClockBehind() (beat int64, last, clock time.Time) {
	return e.beat, e.last, e.clock
}

// fakeStore is a spanbeat.Store standing in for a real chain, so the emitter loop is tested without
// one. It numbers beats one past the last, refuses a beat whose time does not advance, and can be
// told to fail every append or to freeze its clock after the first beat.
type fakeStore struct {
	mu       sync.Mutex
	appended []spanbeat.AppendedBeat
	cadences []int
	beat     int64
	lastAt   time.Time
	failWith error
	freeze   bool
	frozenAt time.Time
}

// AppendSpanBeat records the attempt and returns the beat it wrote, a hard error when configured, or
// a clock-behind error when the supplied time does not advance past the last beat.
func (f *fakeStore) AppendSpanBeat(
	_ context.Context, at time.Time, cadenceSeconds int,
) (spanbeat.AppendedBeat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cadences = append(f.cadences, cadenceSeconds)
	if f.failWith != nil {
		return spanbeat.AppendedBeat{}, f.failWith
	}
	use := at
	if f.freeze {
		if f.frozenAt.IsZero() {
			f.frozenAt = time.Unix(1700000000, 0).UTC()
		}
		use = f.frozenAt
	}
	if !f.lastAt.IsZero() && !use.After(f.lastAt) {
		return spanbeat.AppendedBeat{}, fmt.Errorf("append span beat: %w",
			&clockBehindErr{beat: f.beat + 1, last: f.lastAt, clock: use})
	}
	f.beat++
	f.lastAt = use
	b := spanbeat.AppendedBeat{At: use, Seq: f.beat, Hash: fmt.Sprintf("hash-%d", f.beat), Beat: f.beat}
	f.appended = append(f.appended, b)
	return b, nil
}

// count returns how many beats were written.
func (f *fakeStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.appended)
}

// attempts returns how many appends were tried, successful or not.
func (f *fakeStore) attempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cadences)
}

// snapshot returns a copy of the beats written so far.
func (f *fakeStore) snapshot() []spanbeat.AppendedBeat {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]spanbeat.AppendedBeat(nil), f.appended...)
}

// TestEmitterAppendsBeats verifies the loop beats immediately on start and again on the cadence,
// with consecutive beat numbers and the cadence passed through in whole seconds.
func TestEmitterAppendsBeats(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	emitter := spanbeat.NewEmitter(store, time.Second, nil)
	emitter.Start()
	if !waitFor(t, 3*time.Second, func() bool { return store.count() >= 2 }) {
		emitter.Close()
		t.Fatal("no second beat within three seconds of a one second cadence")
	}
	emitter.Close()

	beats := store.snapshot()
	for i, b := range beats[:2] {
		if b.Beat != int64(i+1) || b.Seq != int64(i+1) || b.Hash == "" {
			t.Errorf("beat %d = %+v, want number and seq %d with a head", i, b, i+1)
		}
	}
	for i, c := range store.cadences {
		if c != 1 {
			t.Errorf("cadence at append %d = %d, want 1", i, c)
		}
	}
}

// TestEmitterAnchorsBeats verifies a configured anchor function is called with each appended beat,
// carrying the seq and hash an anchor commits to, and that an anchor failure does not stop the beats.
func TestEmitterAnchorsBeats(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	anchored := make(chan spanbeat.AppendedBeat, 8)
	emitter := spanbeat.NewEmitter(store, time.Second, nil,
		spanbeat.WithAnchorFunc(func(_ context.Context, b spanbeat.AppendedBeat) error {
			anchored <- b
			return errors.New("authority unreachable")
		}))
	emitter.Start()

	var first spanbeat.AppendedBeat
	select {
	case first = <-anchored:
	case <-time.After(3 * time.Second):
		emitter.Close()
		t.Fatal("the anchor function was never called for the immediate beat")
	}
	if first.Beat != 1 || first.Seq != 1 || first.Hash == "" {
		t.Errorf("anchored beat = %+v, want beat one with the linked head", first)
	}

	// The anchor failed, so the next tick proves the loop carried on beating anyway.
	if !waitFor(t, 3*time.Second, func() bool { return store.count() >= 2 }) {
		emitter.Close()
		t.Fatal("beats stopped after an anchor failure")
	}
	emitter.Close()
}

// TestNoAnchorFuncMeansNoAnchor verifies the emitter never anchors when no anchor function is
// configured. The guard that only anchors against a store that can save anchors lives in the wiring
// now, so the emitter's rule is simply: anchor when told to, never otherwise.
func TestNoAnchorFuncMeansNoAnchor(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	emitter := spanbeat.NewEmitter(store, time.Second, nil)
	emitter.Start()
	if !waitFor(t, 3*time.Second, func() bool { return store.count() >= 1 }) {
		emitter.Close()
		t.Fatal("no beat appended")
	}
	emitter.Close()
}

// TestBeatFailureDoesNotStopLoop verifies an append failure is survived: the loop logs it and tries
// again on the next tick rather than dying, so a transient store outage costs beats, not the emitter.
func TestBeatFailureDoesNotStopLoop(t *testing.T) {
	t.Parallel()
	store := &fakeStore{failWith: errors.New("disk full")}
	emitter := spanbeat.NewEmitter(store, time.Second, nil)
	emitter.Start()
	if !waitFor(t, 3*time.Second, func() bool { return store.attempts() >= 2 }) {
		emitter.Close()
		t.Fatal("the loop did not try again after a failed beat")
	}
	emitter.Close()
	if store.count() != 0 {
		t.Errorf("a failing store recorded %d beats, want 0", store.count())
	}
}

// TestBeatWarnsWhenTheClockMovedBackward verifies the operator is told when a beat was refused
// because this machine's clock had not passed the last beat. The record itself shows only a reported
// gap, which is the honest thing for it to show, so the warning is how an operator learns the clock
// is the reason rather than the collector dying.
func TestBeatWarnsWhenTheClockMovedBackward(t *testing.T) {
	t.Parallel()
	core, logs := observer.New(zap.WarnLevel)

	// A beat recorded at the time it was asked for warns about nothing, so the check is not simply
	// warning on every beat.
	quietStore := &fakeStore{}
	quiet := spanbeat.NewEmitter(quietStore, time.Second, zap.New(core))
	quiet.Start()
	if !waitFor(t, 3*time.Second, func() bool { return quietStore.count() >= 2 }) {
		quiet.Close()
		t.Fatal("no ordinary beats appended")
	}
	quiet.Close()
	if logs.Len() != 0 {
		t.Fatalf("ordinary beats logged %v, want no warning", logs.All())
	}

	moved := spanbeat.NewEmitter(&fakeStore{freeze: true}, time.Second, zap.New(core))
	moved.Start()
	if !waitFor(t, 3*time.Second, func() bool { return logs.Len() > 0 }) {
		moved.Close()
		t.Fatal("a refused beat produced no warning, so an operator never learns the clock is why " +
			"the heartbeat went quiet")
	}
	moved.Close()
	entry := logs.All()[0]
	if !strings.Contains(entry.Message, "has not passed the last beat") {
		t.Errorf("warning = %q, want it to say the clock has not passed the last beat", entry.Message)
	}
	// The structured fields the emitter logs come straight from the store's ClockBehind report.
	fields := entry.ContextMap()
	if beat, ok := fields["beat"].(int64); !ok || beat < 2 {
		t.Errorf("warning beat field = %v, want the refused beat number", fields["beat"])
	}
}

// TestNewEmitterPanics pins the constructor contract: a nil store or a cadence that is not a whole
// number of seconds of at least one is a developer error, and a nil logger is not.
func TestNewEmitterPanics(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Store    spanbeat.Store
		Cadence  time.Duration
		WantBoom bool
	}{{ // Test 0: A nil store panics.
		Store: nil, Cadence: time.Second, WantBoom: true,
	}, { // Test 1: A zero cadence panics.
		Store: &fakeStore{}, Cadence: 0, WantBoom: true,
	}, { // Test 2: A sub-second cadence panics.
		Store: &fakeStore{}, Cadence: 500 * time.Millisecond, WantBoom: true,
	}, { // Test 3: A fractional cadence panics, the entry records whole seconds.
		Store: &fakeStore{}, Cadence: 1500 * time.Millisecond, WantBoom: true,
	}, { // Test 4: A whole-second cadence with a nil logger is fine.
		Store: &fakeStore{}, Cadence: time.Minute, WantBoom: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if boom := recover() != nil; boom != test.WantBoom {
					t.Errorf("NewEmitter() panic = %v, want %v", boom, test.WantBoom)
				}
			}()
			spanbeat.NewEmitter(test.Store, test.Cadence, nil)
		})
	}
}

// TestReadStatsCountsBeats pins the counters the metrics endpoint alerts on. Going quiet is only a
// bounded gap in the record, so it has to be loud here: a suppressed beat that never moved the
// counter would leave the operator alerting on nothing. The counters are process-wide, so the test
// asserts deltas rather than absolutes and stays safe beside the other parallel tests.
func TestReadStatsCountsBeats(t *testing.T) {
	t.Parallel()
	before := spanbeat.ReadStats()

	appending := &fakeStore{}
	ordinary := spanbeat.NewEmitter(appending, time.Second, nil)
	ordinary.Start()
	if !waitFor(t, 3*time.Second, func() bool { return appending.count() >= 2 }) {
		ordinary.Close()
		t.Fatal("no beats appended")
	}
	ordinary.Close()

	frozen := &fakeStore{freeze: true}
	refusing := spanbeat.NewEmitter(frozen, time.Second, nil)
	refusing.Start()
	if !waitFor(t, 3*time.Second, func() bool { return frozen.attempts() >= 2 }) {
		refusing.Close()
		t.Fatal("no second append attempt against the frozen clock")
	}
	refusing.Close()

	after := spanbeat.ReadStats()
	if got := after.Appended - before.Appended; got < 2 {
		t.Errorf("Appended rose by %d, want at least the 2 beats this test wrote", got)
	}
	if got := after.Suppressed - before.Suppressed; got < 1 {
		t.Errorf("Suppressed rose by %d, want at least the 1 refused beat", got)
	}
	// Last is asserted only as set, not as ordered: the frozen-clock fakes in this file write a
	// fixed past instant into the shared counter, so under the parallel suite its ordering is
	// whichever emitter wrote last, and asserting more than presence races with the sibling tests.
	if after.Last.IsZero() {
		t.Error("Last is zero after beats were appended, so the metrics endpoint would report a " +
			"process that never beat")
	}
}
