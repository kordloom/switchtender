package dispatch

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAClaimGateStopsNewWorkWithoutKillingTheProcess covers a paid feature whose gate ran once.
//
// A relay worker exists to run distributed execution, and that is licensed. The check happened at
// startup and the process then sat on a signal for as long as the operator left it up, so a term
// that lapsed months later still had a fleet of workers draining the queue. The evidence emitter had
// exactly this shape and now reads its license on every tick.
//
// What a refusal must not do is kill the daemon. Runs already executing finish, the process stays
// up, and only new claims stop, because a lapse that takes down a running install is the outcome
// every other lapse path here was fixed to avoid.
func TestAClaimGateStopsNewWorkWithoutKillingTheProcess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	var executed atomic.Int64
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			executed.Add(1)
			return roundhouse.Result{ExitCode: 0}, nil
		})

	var allowed atomic.Bool
	allowed.Store(true)
	refused := errors.New("distributed workers need a license this install does not have")
	d := New(store, runner, nil, WithClaimGate(func() error {
		if allowed.Load() {
			return nil
		}
		return refused
	}))
	defer d.Close()

	// Licensed: work is claimed and run.
	if _, err := d.Submit(ctx, "site.yml", "inv"); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitFor(t, func() bool { return executed.Load() == 1 })

	// The term runs out while the process keeps running.
	allowed.Store(false)
	before := executed.Load()
	if _, err := d.Submit(ctx, "site.yml", "inv"); err != nil {
		t.Fatalf("Submit() after the lapse error = %v: submitting is not what the gate stops", err)
	}
	// Long enough that the loop would have claimed it several times over.
	time.Sleep(300 * time.Millisecond)
	if got := executed.Load(); got != before {
		t.Errorf("the worker ran %d runs after its license lapsed, was %d: the gate is checked "+
			"once at startup and a process that lives for months never reads it again", got, before)
	}

	// And it resumes rather than needing a restart, because the loop asks every time.
	allowed.Store(true)
	waitFor(t, func() bool { return executed.Load() > before })
}

// waitFor polls until the condition holds or the test's patience runs out, so a loop's timing does
// not decide whether this passes.
func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the condition never held")
}
