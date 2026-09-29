package schedule

import (
	"runtime"
	"testing"
	"time"
)

// TestCloseWithoutStartReturns pins that stopping a scheduler that was never started returns.
//
// Close waits on a channel the loop closes on its way out, and nothing else closes it, so a Close
// on a scheduler whose Start never ran waited forever. Every caller wires Start and a deferred
// Close together today, but a startup that gives up between the two, or a caller that only ever
// wanted the scheduler built, turns that wait into a process that never finishes shutting down and
// a goroutine that holds the store and the dispatcher alive with it.
func TestCloseWithoutStartReturns(t *testing.T) {
	t.Parallel()
	s := NewScheduler(NewMemStore(), &failingSubmitter{}, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Close()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close() on a scheduler that was never started did not return")
	}
}

// TestSecondStartDoesNotLaunchASecondLoop pins that Start is idempotent.
//
// Two loops over one store would race each other's ClaimDue on every due row, and whichever exited
// second would close done twice, which panics and takes the process with it. The goroutine count is
// what says whether the second call launched anything.
func TestSecondStartDoesNotLaunchASecondLoop(t *testing.T) {
	s := NewScheduler(NewMemStore(), &failingSubmitter{}, nil, WithInterval(time.Hour))
	s.Start()
	settled := runtime.NumGoroutine()
	s.Start()
	s.Start()
	if got := runtime.NumGoroutine(); got != settled {
		t.Errorf("goroutines after two more Start() calls = %d, want %d: Start launched another loop",
			got, settled)
	}
	s.Close()
}

// TestCloseIsIdempotent pins that stopping twice is safe, since a shutdown path that runs both a
// deferred Close and an explicit one must not panic on the second.
func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	s := NewScheduler(NewMemStore(), &failingSubmitter{}, nil, WithInterval(time.Hour))
	s.Start()
	s.Close()
	s.Close()
}
