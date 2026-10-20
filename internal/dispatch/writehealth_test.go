package dispatch

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// TestWriteHealthIsDegradedForAWindow pins readiness's window on an injected clock: degraded from a
// failed write until five minutes after it, and clean after, and degraded for as long as an end is
// owed regardless of the window.
func TestWriteHealthIsDegradedForAWindow(t *testing.T) {
	t.Parallel()
	failed := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		At           time.Duration
		Owed         bool
		WantDegraded bool
		WantReasons  int
	}{{ // Test 0: At the failure.
		At: 0, WantDegraded: true, WantReasons: 1,
	}, { // Test 1: Just inside the window.
		At: degradedWindow - time.Second, WantDegraded: true, WantReasons: 1,
	}, { // Test 2: At the window's end it clears on its own.
		At: degradedWindow, WantDegraded: false,
	}, { // Test 3: An owed end keeps it degraded past the window.
		At: time.Hour, Owed: true, WantDegraded: true, WantReasons: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var h writeHealth
			h.noteFailure(failed, errors.New("disk full"))
			if test.Owed {
				h.owe(&run.Run{ID: "run_1"}, run.Finalization{}, errors.New("disk full"))
			}
			got := h.snapshot(failed.Add(test.At))
			if got.Degraded != test.WantDegraded || len(got.Reasons) != test.WantReasons {
				t.Errorf("snapshot = %+v, want degraded %v with %d reason(s)", got, test.WantDegraded,
					test.WantReasons)
			}
		})
	}
}

// TestOwedEndsAreBounded pins that a store refusing for a long time cannot grow the owed ends
// without bound: past the limit the oldest is dropped and named, and it ends interrupted through
// the janitor as it did before ends were held.
func TestOwedEndsAreBounded(t *testing.T) {
	t.Parallel()
	var h writeHealth
	refused := errors.New("disk full")
	for i := range maxOwedEnds {
		r := &run.Run{ID: fmt.Sprintf("run_%d", i)}
		if dropped := h.owe(r, run.Finalization{}, refused); dropped != "" {
			t.Fatalf("owe %d dropped %q inside the bound", i, dropped)
		}
	}
	if dropped := h.owe(&run.Run{ID: "run_last"}, run.Finalization{}, refused); dropped != "run_0" {
		t.Errorf("owe past the bound dropped %q, want run_0", dropped)
	}
	if n := len(h.pending()); n != maxOwedEnds {
		t.Errorf("pending = %d, want %d", n, maxOwedEnds)
	}
	h.settle("run_last")
	if n := len(h.pending()); n != maxOwedEnds-1 {
		t.Errorf("pending after settle = %d, want %d", n, maxOwedEnds-1)
	}
}

// appendRefusingStore refuses every log append.
type appendRefusingStore struct {
	run.Store
}

// AppendLog refuses.
func (appendRefusingStore) AppendLog(context.Context, string, []byte) error {
	return errors.New("database or disk is full (13)")
}

// TestAFailedAppendStampsWriteHealth pins that a log append the store refuses after its retries
// marks the process degraded, so readiness says so while the run is still going and not only once
// it ends.
func TestAFailedAppendStampsWriteHealth(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	var h writeHealth
	s := &logSink{store: appendRefusingStore{Store: run.NewMemStore()}, id: "run_1", log: zap.NewNop(),
		publisher: noopPublisher{}, health: &h, now: func() time.Time { return at }}
	s.emit([]byte("output"))
	if got := h.snapshot(at); !got.Degraded || !got.LastWriteFailure.Equal(at) {
		t.Errorf("snapshot = %+v, want degraded with the failure at %s", got, at)
	}
}
