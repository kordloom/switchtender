package dispatch

import (
	"context"
	"io"
	"runtime"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// quietRunner succeeds immediately and writes one line, so a lifecycle test exercises the execution
// path without depending on a tool being installed.
type quietRunner struct{}

// Run writes a line and succeeds.
func (quietRunner) Run(_ context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result,
	error) {
	_, _ = io.WriteString(out, "ran "+spec.Playbook)
	return roundhouse.Result{ExitCode: 0}, nil
}

// settledGoroutines returns the goroutine count once it has stopped falling, or the last reading
// after the wait runs out. Goroutines that are on their way out are not leaks, so a leak check that
// read the count the instant Close returned would fail on timing rather than on a defect.
func settledGoroutines(base int) int {
	got := runtime.NumGoroutine()
	for i := 0; i < 100 && got > base; i++ {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
		got = runtime.NumGoroutine()
	}
	return got
}

// leakCycles is how many dispatchers a lifecycle check builds and closes. It is well above the
// tolerance below, so a component that leaks even one goroutine per lifecycle is caught.
const leakCycles = 25

// leakTolerance is how much drift a lifecycle check accepts. Other tests in this package leave
// their own goroutines settling, so the check is that the count does not grow with the number of
// cycles, not that it lands on an exact number.
const leakTolerance = 5

// TestDispatcherLifecycleLeavesNoGoroutines pins that a dispatcher gives back every goroutine it
// took when it is closed.
//
// A dispatcher owns a claim loop, a janitor, an inventory sync loop, a lease watcher and an event
// tailer per run, and a delivery goroutine per notification target. Close cancels the context and
// waits on both wait groups, and the whole contract rests on every one of those goroutines being
// tracked by one of them. A control plane that keeps one back per run is fine in a test and dead in
// a week, so this counts them across many lifecycles rather than one.
func TestDispatcherLifecycleLeavesNoGoroutines(t *testing.T) {
	base := runtime.NumGoroutine()
	for i := 0; i < leakCycles; i++ {
		d := New(run.NewMemStore(), quietRunner{}, nil, WithClaimInterval(time.Millisecond))
		if _, err := d.Submit(context.Background(), "site.yml", "hosts.ini"); err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
		d.Close()
	}
	if got := settledGoroutines(base); got > base+leakTolerance {
		t.Errorf("goroutines after %d dispatcher lifecycles = %d, want at most %d: a dispatcher is "+
			"keeping goroutines back after Close", leakCycles, got, base+leakTolerance)
	}
}

// TestDispatcherCloseIsIdempotent pins that a shutdown path running both a deferred Close and an
// explicit one does not panic or hang on the second.
func TestDispatcherCloseIsIdempotent(t *testing.T) {
	d := New(run.NewMemStore(), quietRunner{}, nil, WithNoJanitor(),
		WithClaimInterval(time.Millisecond))
	d.Close()
	d.Close()
}

// TestFinishedRunsLeaveNoCancelEntries pins that the map of in-flight cancel funcs empties out.
//
// Every executing run, split parent and pipeline parent registers a cancel func under its id so an
// operator's cancel can reach it, and every one of them must be unregistered when the run settles.
// An entry left behind holds the run's context alive and grows the map for the life of the process,
// which is the same one-per-run growth that kills a long-lived controller.
func TestFinishedRunsLeaveNoCancelEntries(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	d := New(store, quietRunner{}, nil, WithNoJanitor(), WithClaimInterval(time.Millisecond))
	defer d.Close()

	for i := 0; i < 10; i++ {
		r, err := d.Submit(context.Background(), "site.yml", "hosts.ini")
		if err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
		waitTerminal(t, store, r.ID)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		d.cmu.Lock()
		held := len(d.cancels)
		d.cmu.Unlock()
		if held == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancel entries still held after every run settled = %d, want 0", held)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
