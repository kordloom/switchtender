package spanbeat_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/spanbeat"
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

// chainLen returns the audit chain length, failing the test on a read error.
func chainLen(t *testing.T, store audit.Store) int {
	t.Helper()
	chain, err := store.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	return len(chain)
}

// TestEmitterAppendsBeats verifies the loop beats immediately on start and again on the cadence,
// with consecutive beat numbers and the cadence recorded in each entry.
func TestEmitterAppendsBeats(t *testing.T) {
	t.Parallel()
	store := audit.NewMemStore()
	emitter := spanbeat.NewEmitter(store, time.Second, nil)
	emitter.Start()
	if !waitFor(t, 3*time.Second, func() bool { return chainLen(t, store) >= 2 }) {
		emitter.Close()
		t.Fatal("no second beat within three seconds of a one second cadence")
	}
	emitter.Close()

	chain, err := store.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	for i, e := range chain[:2] {
		beat, count, cadence, ok := audit.ParseSpanPath(e.Path)
		if !ok || e.Actor != audit.SpanActor || e.Method != audit.SpanMethod {
			t.Fatalf("entry %d is not a span beat: %+v", i, e)
		}
		if beat != int64(i+1) || count != 0 || cadence != 1 {
			t.Errorf("entry %d beat = %d count = %d cadence = %d, want %d 0 1",
				i, beat, count, cadence, i+1)
		}
	}
	if ok, at := audit.Verify(chain); !ok {
		t.Errorf("Verify() reported a break at %d", at)
	}
}

// TestEmitterAnchorsBeats verifies a configured anchor function is called with each appended beat
// when the store keeps anchors, and that an anchor failure does not stop the beats.
func TestEmitterAnchorsBeats(t *testing.T) {
	t.Parallel()
	store := audit.NewMemStore()
	anchored := make(chan *audit.Entry, 8)
	emitter := spanbeat.NewEmitter(store, time.Second, nil,
		spanbeat.WithAnchorFunc(func(_ context.Context, e *audit.Entry) error {
			anchored <- e
			return errors.New("authority unreachable")
		}))
	emitter.Start()

	var first *audit.Entry
	select {
	case first = <-anchored:
	case <-time.After(3 * time.Second):
		emitter.Close()
		t.Fatal("the anchor function was never called for the immediate beat")
	}
	if beat, _, _, ok := audit.ParseSpanPath(first.Path); !ok || beat != 1 {
		t.Errorf("anchored entry path = %q, want beat one", first.Path)
	}
	if first.Hash == "" || first.Seq != 1 {
		t.Errorf("anchored entry seq = %d hash = %q, want the linked head", first.Seq, first.Hash)
	}

	// The anchor failed, so the next tick proves the loop carried on beating anyway.
	if !waitFor(t, 3*time.Second, func() bool { return chainLen(t, store) >= 2 }) {
		emitter.Close()
		t.Fatal("beats stopped after an anchor failure")
	}
	emitter.Close()
}

// noAnchorStore hides the mem store's anchor methods, standing in for a backend that keeps no
// anchors.
type noAnchorStore struct {
	// Store is the wrapped audit store.
	audit.Store
}

// TestAnchorSkippedWhenStoreKeepsNoAnchors verifies the anchor function is never called against a
// store that cannot save what it produces, since an anchor that is not stored proves nothing.
func TestAnchorSkippedWhenStoreKeepsNoAnchors(t *testing.T) {
	t.Parallel()
	store := noAnchorStore{Store: audit.NewMemStore()}
	var calls atomic.Int64
	emitter := spanbeat.NewEmitter(store, time.Second, nil,
		spanbeat.WithAnchorFunc(func(context.Context, *audit.Entry) error {
			calls.Add(1)
			return nil
		}))
	emitter.Start()
	if !waitFor(t, 3*time.Second, func() bool { return chainLen(t, store) >= 1 }) {
		emitter.Close()
		t.Fatal("no beat appended")
	}
	emitter.Close()
	if n := calls.Load(); n != 0 {
		t.Errorf("anchor function called %d times against a store keeping no anchors, want 0", n)
	}
}

// failingStore is an audit store whose span appends always fail, standing in for a full disk or an
// unreachable database.
type failingStore struct {
	// Store is embedded so only the failing method needs defining.
	audit.Store
	// calls counts span append attempts.
	calls atomic.Int64
}

// AppendSpanBeat counts the attempt and fails.
func (f *failingStore) AppendSpanBeat(context.Context, time.Time, int) (*audit.Entry, error) {
	f.calls.Add(1)
	return nil, errors.New("disk full")
}

// TestBeatFailureDoesNotStopLoop verifies an append failure is survived: the loop logs it and
// tries again on the next tick rather than dying, so a transient store outage costs beats, not the
// emitter.
func TestBeatFailureDoesNotStopLoop(t *testing.T) {
	t.Parallel()
	store := &failingStore{Store: audit.NewMemStore()}
	emitter := spanbeat.NewEmitter(store, time.Second, nil)
	emitter.Start()
	if !waitFor(t, 3*time.Second, func() bool { return store.calls.Load() >= 2 }) {
		emitter.Close()
		t.Fatal("the loop did not try again after a failed beat")
	}
	emitter.Close()
}

// stoppedClockStore hands the store the same instant for every beat, which is what a wall clock
// that stepped backward or stopped looks like from the emitter's side. The first beat lands, and
// every beat after it fails to advance past that one, so the store refuses it.
type stoppedClockStore struct {
	// Store is embedded so only the frozen-time method needs defining.
	audit.Store
}

// AppendSpanBeat appends every beat at one fixed instant, ignoring the time it was handed.
func (s stoppedClockStore) AppendSpanBeat(ctx context.Context, _ time.Time, cadenceS int) (*audit.Entry, error) {
	return s.Store.AppendSpanBeat(ctx, time.Unix(1700000000, 0).UTC(), cadenceS)
}

// TestBeatWarnsWhenTheClockMovedBackward verifies the operator is told when a beat was refused
// because this machine's clock had not passed the last beat. The record itself shows only a
// reported gap, which is the honest thing for it to show, so the warning is how an operator learns
// the clock is the reason rather than the collector dying.
func TestBeatWarnsWhenTheClockMovedBackward(t *testing.T) {
	t.Parallel()
	core, logs := observer.New(zap.WarnLevel)

	// A beat recorded at the time it was asked for warns about nothing, so the check is not simply
	// warning on every beat.
	ordinary := audit.NewMemStore()
	quiet := spanbeat.NewEmitter(ordinary, time.Second, zap.New(core))
	quiet.Start()
	if !waitFor(t, 3*time.Second, func() bool { return chainLen(t, ordinary) >= 2 }) {
		quiet.Close()
		t.Fatal("no beats appended")
	}
	quiet.Close()
	if logs.Len() != 0 {
		t.Fatalf("ordinary beats logged %v, want no warning", logs.All())
	}

	moved := spanbeat.NewEmitter(stoppedClockStore{Store: audit.NewMemStore()}, time.Second, zap.New(core))
	moved.Start()
	if !waitFor(t, 3*time.Second, func() bool { return logs.Len() > 0 }) {
		moved.Close()
		t.Fatal("a refused beat produced no warning, so an operator never learns the clock is why " +
			"the heartbeat went quiet")
	}
	moved.Close()
	if got := logs.All()[0].Message; !strings.Contains(got, "has not passed the last beat") {
		t.Errorf("warning = %q, want it to say the clock has not passed the last beat", got)
	}
}

// TestNewEmitterPanics pins the constructor contract: a nil store or a cadence that is not a whole
// number of seconds of at least one is a developer error, and a nil logger is not.
func TestNewEmitterPanics(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Store    audit.Store
		Cadence  time.Duration
		WantBoom bool
	}{{ // Test 0: A nil store panics.
		Store: nil, Cadence: time.Second, WantBoom: true,
	}, { // Test 1: A zero cadence panics.
		Store: audit.NewMemStore(), Cadence: 0, WantBoom: true,
	}, { // Test 2: A sub-second cadence panics.
		Store: audit.NewMemStore(), Cadence: 500 * time.Millisecond, WantBoom: true,
	}, { // Test 3: A fractional cadence panics, the entry records whole seconds.
		Store: audit.NewMemStore(), Cadence: 1500 * time.Millisecond, WantBoom: true,
	}, { // Test 4: A whole-second cadence with a nil logger is fine.
		Store: audit.NewMemStore(), Cadence: time.Minute, WantBoom: false,
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
