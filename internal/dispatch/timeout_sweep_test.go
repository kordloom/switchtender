package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// sleepingRunner returns a runner that works for d and then succeeds, or reports its context's error
// if the run is stopped first. It is how a test tells a run that was allowed to finish from one that
// was cut short.
func sleepingRunner(d time.Duration) roundhouse.Runner {
	return roundhouse.RunnerFunc(
		func(ctx context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			select {
			case <-time.After(d):
				return roundhouse.Result{ExitCode: 0}, nil
			case <-ctx.Done():
				return roundhouse.Result{ExitCode: -1}, ctx.Err()
			}
		})
}

// TestPerRunTimeoutOverridesTheServerDefault pins the precedence between the two bounds, in the
// direction no existing test reads. A run that asks for longer than the server default must get it:
// the per-run value replaces the default rather than being clamped by it, so a job an operator knows
// takes an hour is not cut off at the default. Clamping instead would kill long changes partway
// through, and the run would be recorded as a timeout nobody configured.
func TestPerRunTimeoutOverridesTheServerDefault(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which pair of bounds is in play.
		Name string
		// ServerTimeout is the dispatcher-wide cap.
		ServerTimeout time.Duration
		// RunTimeout is the seconds the run itself asks for; zero means it asks for nothing.
		RunTimeout int
		// WantStatus is the terminal status the run should reach.
		WantStatus run.Status
	}{{ // Test 0: A generous per-run timeout beats a tight server default and the run finishes.
		Name: "a longer per-run timeout wins", ServerTimeout: 80 * time.Millisecond,
		RunTimeout: 3600, WantStatus: run.StatusSucceeded,
	}, { // Test 1: A run asking for nothing falls back to the server default, which stops it.
		Name: "no per-run timeout falls back to the default", ServerTimeout: 80 * time.Millisecond,
		RunTimeout: 0, WantStatus: run.StatusFailed,
	}, { // Test 2: No cap anywhere lets the run finish on its own.
		Name: "no cap at all", ServerTimeout: 0, RunTimeout: 0, WantStatus: run.StatusSucceeded,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := run.NewMemStore()
			d := New(store, sleepingRunner(400*time.Millisecond), nil, WithNoJanitor(),
				WithRunTimeout(test.ServerTimeout), WithClaimInterval(time.Millisecond))
			defer d.Close()

			opts := []run.SubmitOption{run.WithTool(run.ToolBash), run.WithCommand("deploy")}
			if test.RunTimeout > 0 {
				opts = append(opts, run.WithTimeout(test.RunTimeout))
			}
			created, err := d.Submit(context.Background(), "", "", opts...)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			got := waitTerminal(t, store, created.ID)
			if got.Status != test.WantStatus {
				t.Errorf("status = %q, want %q (server cap %v, run asked for %ds)",
					got.Status, test.WantStatus, test.ServerTimeout, test.RunTimeout)
			}
		})
	}
}

// TestATimedOutRunIsFailedRatherThanCanceled pins how the executor classifies its own timeout. The
// context is canceled either way, so without the cause a timeout was indistinguishable from a person
// clicking cancel: the record would say somebody stopped the change when in fact the tool overran a
// bound the operator set, and nobody would know to raise it.
func TestATimedOutRunIsFailedRatherThanCanceled(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	d := New(store, sleepingRunner(10*time.Second), nil, WithNoJanitor(),
		WithRunTimeout(100*time.Millisecond), WithClaimInterval(time.Millisecond))
	defer d.Close()

	created, err := d.Submit(context.Background(), "", "",
		run.WithTool(run.ToolBash), run.WithCommand("deploy"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	got := waitTerminal(t, store, created.ID)

	if got.Status == run.StatusCanceled {
		t.Fatal("a run stopped by its own timeout was recorded as canceled, which is the record a " +
			"person clicking cancel leaves")
	}
	if got.Status != run.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "timeout") {
		t.Errorf("error = %q, want it to name the timeout as the cause", got.Error)
	}
}

// TestSettleOverrunningBoundaries walks the sweep's guards one at a time. It is the only bound on a
// worker that keeps its lease fresh while never finishing, so a relay that claimed work and
// heartbeated forever could otherwise hold the queue indefinitely. Each guard here is a run the sweep
// must leave alone, and the last is the one it must end.
func TestSettleOverrunningBoundaries(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		// Name says which guard the run exercises.
		Name string
		// Timeout is the run's own bound in seconds.
		Timeout int
		// Started is when the run began, or nil when it never recorded a start.
		Started *time.Time
		// WantStatus is the status the sweep should leave the run in.
		WantStatus run.Status
	}{{ // Test 0: Exactly at the deadline the run has overrun, so the sweep ends it.
		Name: "exactly at the deadline", Timeout: 60,
		Started: ptr(now.Add(-(60*time.Second + overrunGrace))), WantStatus: run.StatusFailed,
	}, { // Test 1: One nanosecond short of the deadline is still inside the grace.
		Name: "one nanosecond short", Timeout: 60,
		Started:    ptr(now.Add(-(60*time.Second + overrunGrace) + time.Nanosecond)),
		WantStatus: run.StatusRunning,
	}, { // Test 2: A running run that never recorded a start has no deadline to measure from.
		Name: "no recorded start", Timeout: 60, Started: nil, WantStatus: run.StatusRunning,
	}, { // Test 3: A negative timeout is not a bound, so it is left to its executor.
		Name: "a negative timeout", Timeout: -30, Started: ptr(now.Add(-24 * time.Hour)),
		WantStatus: run.StatusRunning,
	}, { // Test 4: A one-second timeout overrun by a day is ended, however small the bound was.
		Name: "the smallest real timeout", Timeout: 1, Started: ptr(now.Add(-24 * time.Hour)),
		WantStatus: run.StatusFailed,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			fresh := now.Add(-time.Second)
			r := &run.Run{
				ID: fmt.Sprintf("run_%d", testNum), Playbook: "site.yml", Status: run.StatusRunning,
				CreatedAt: now.Add(-25 * time.Hour), StartedAt: test.Started,
				ClaimedBy: "worker-a", ClaimedAt: &fresh, Timeout: test.Timeout,
			}
			if err := store.Save(ctx, r); err != nil {
				t.Fatalf("Save() error = %v", err)
			}

			d := &Dispatcher{
				store: store, log: zap.NewNop(), ctx: ctx,
				now: func() time.Time { return now },
			}
			d.settleOverrunning()

			got, err := store.Get(ctx, r.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.Status != test.WantStatus {
				t.Errorf("status = %q, want %q", got.Status, test.WantStatus)
			}
			if test.WantStatus != run.StatusFailed {
				return
			}
			if !strings.Contains(got.Error, "timed out") {
				t.Errorf("error = %q, want it to say the run timed out", got.Error)
			}
			if !strings.Contains(got.Error, fmt.Sprintf("%ds timeout", test.Timeout)) {
				t.Errorf("error = %q, want it to name the timeout it exceeded", got.Error)
			}
			if got.EndedAt == nil {
				t.Error("the settled run records no end time, so its duration is unreadable")
			}
		})
	}
}

// ptr returns a pointer to v, for building the optional timestamps a run record carries.
func ptr[T any](v T) *T { return &v }

// listFailStore refuses the running-run listing the overrun sweep starts from.
type listFailStore struct {
	run.Store
	// calls counts refused listings.
	calls atomic.Int64
}

// ListPage refuses every listing.
func (s *listFailStore) ListPage(context.Context, run.ListFilter, int, int) ([]*run.Run, error) {
	s.calls.Add(1)
	return nil, errors.New("database is locked")
}

// finalizeFailStore refuses the fenced terminal write the overrun sweep uses.
type finalizeFailStore struct {
	run.Store
	// calls counts refused writes.
	calls atomic.Int64
}

// FinalizeRunning refuses every write.
func (s *finalizeFailStore) FinalizeRunning(context.Context, string, run.Finalization) (bool, error) {
	s.calls.Add(1)
	return false, errors.New("database is locked")
}

// TestSettleOverrunningSurvivesAStoreFailure pins that a sweep which cannot read or cannot write
// leaves the run where it was rather than acting on an unknown state. The sweep runs on a timer, so
// giving up quietly and retrying on the next tick is right; what would be wrong is treating an
// unreadable fleet as an empty one, or a refused write as a run that ended.
func TestSettleOverrunningSurvivesAStoreFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	started := now.Add(-2 * time.Hour)
	fresh := now.Add(-time.Second)

	overrun := func() *run.Run {
		return &run.Run{
			ID: "run_overrun", Playbook: "site.yml", Status: run.StatusRunning,
			CreatedAt: started, StartedAt: &started, ClaimedBy: "worker-a", ClaimedAt: &fresh,
			Timeout: 60,
		}
	}

	t.Run("test 0", func(t *testing.T) {
		t.Parallel()
		// Test 0: The listing fails, so the sweep does nothing and leaves the run for the next tick.
		ctx := context.Background()
		base := run.NewMemStore()
		if err := base.Save(ctx, overrun()); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		store := &listFailStore{Store: base}
		d := &Dispatcher{store: store, log: zap.NewNop(), ctx: ctx,
			now: func() time.Time { return now }}
		d.settleOverrunning()

		if store.calls.Load() == 0 {
			t.Fatal("the listing was never attempted")
		}
		got, err := base.Get(ctx, "run_overrun")
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if got.Status != run.StatusRunning {
			t.Errorf("status = %q, want it untouched: the sweep could not read the fleet", got.Status)
		}
	})

	t.Run("test 1", func(t *testing.T) {
		t.Parallel()
		// Test 1: The terminal write is refused, so the run stays running for the sweep to retry.
		ctx := context.Background()
		base := run.NewMemStore()
		if err := base.Save(ctx, overrun()); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		store := &finalizeFailStore{Store: base}
		d := &Dispatcher{store: store, log: zap.NewNop(), ctx: ctx,
			now: func() time.Time { return now }}
		d.settleOverrunning()

		if store.calls.Load() == 0 {
			t.Fatal("the terminal write was never attempted")
		}
		got, err := base.Get(ctx, "run_overrun")
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if got.Status != run.StatusRunning {
			t.Errorf("status = %q, want it left running: a refused write is not a run that ended",
				got.Status)
		}
	})
}

// TestSettleOverrunningEndsOnlyWhatOverran runs the sweep over a mixed fleet in one pass, which is
// how it actually runs. A guard that works in isolation but stops the loop, or that settles the wrong
// row, only shows up when several runs are present at once.
func TestSettleOverrunningEndsOnlyWhatOverran(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	long := now.Add(-2 * time.Hour)
	fresh := now.Add(-time.Second)

	runs := []*run.Run{{
		ID: "run_over_1", Playbook: "a.yml", Status: run.StatusRunning, CreatedAt: long,
		StartedAt: &long, ClaimedBy: "w", ClaimedAt: &fresh, Timeout: 60,
	}, {
		ID: "run_healthy", Playbook: "b.yml", Status: run.StatusRunning, CreatedAt: long,
		StartedAt: &long, ClaimedBy: "w", ClaimedAt: &fresh, Timeout: 86400,
	}, {
		ID: "run_over_2", Playbook: "c.yml", Status: run.StatusRunning, CreatedAt: long,
		StartedAt: &long, ClaimedBy: "w", ClaimedAt: &fresh, Timeout: 30,
	}, {
		ID: "run_done", Playbook: "d.yml", Status: run.StatusSucceeded, CreatedAt: long,
		StartedAt: &long, EndedAt: &now, ClaimedBy: "w", ClaimedAt: &fresh, Timeout: 1,
	}}
	for _, r := range runs {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}

	d := &Dispatcher{store: store, log: zap.NewNop(), ctx: ctx, now: func() time.Time { return now }}
	d.settleOverrunning()

	want := map[string]run.Status{
		"run_over_1":  run.StatusFailed,
		"run_healthy": run.StatusRunning,
		"run_over_2":  run.StatusFailed,
		"run_done":    run.StatusSucceeded,
	}
	for id, wantStatus := range want {
		got, err := store.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", id, err)
		}
		if got.Status != wantStatus {
			t.Errorf("%s = %q, want %q", id, got.Status, wantStatus)
		}
	}
}

// getFailingStore refuses reads for one named id and serves every other from the wrapped store.
type getFailingStore struct {
	run.Store
	// badID is the run whose read always fails.
	badID string
}

// Get refuses the named run and serves the rest.
func (s *getFailingStore) Get(ctx context.Context, id string) (*run.Run, error) {
	if id == s.badID {
		return nil, errors.New("database is locked")
	}
	return s.Store.Get(ctx, id)
}

// TestCommitSettledSkipsWhatItCannotReadAndRecordsTheRest pins the sweep's evidence pass as best
// effort per run rather than all or nothing. These runs have already happened, so refusing to record
// any of them because one row could not be read would lose the evidence for the others, and the runs
// most likely to be unreadable are the ones an incident is about.
func TestCommitSettledSkipsWhatItCannotReadAndRecordsTheRest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := run.NewMemStore()
	audits := audit.NewMemStore()

	ended := time.Now()
	for _, id := range []string{"run_readable", "run_unreadable"} {
		if err := base.Save(ctx, &run.Run{
			ID: id, Tool: run.ToolBash, Command: "deploy", Status: run.StatusInterrupted,
			CreatedAt: ended, StartedAt: &ended, EndedAt: &ended, Actor: "casey",
		}); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
	}

	d := &Dispatcher{
		store:  &getFailingStore{Store: base, badID: "run_unreadable"},
		audits: audits, log: zap.NewNop(), ctx: ctx, now: time.Now,
	}
	d.commitSettled([]string{"run_unreadable", "run_readable"})

	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	var readable, unreadable int
	for _, e := range chain {
		switch {
		case strings.Contains(e.Path, "run_readable"):
			readable++
		case strings.Contains(e.Path, "run_unreadable"):
			unreadable++
		}
	}
	if readable == 0 {
		t.Error("a readable swept run left no chain entry, so one unreadable row lost the evidence " +
			"for every run in the same sweep")
	}
	if unreadable != 0 {
		t.Errorf("%d entries were committed for a run whose record could not be read, so the chain "+
			"asserts an outcome nothing can recompute", unreadable)
	}
}

// TestCommitSettledDoesNothingWithoutAChain pins the two no-op guards. An install that keeps no audit
// trail must not have the sweep read every settled run back out of the store for nothing, and an
// empty sweep must not touch the store at all.
func TestCommitSettledDoesNothingWithoutAChain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := &readCountingStore{Store: run.NewMemStore()}

	t.Run("test 0", func(t *testing.T) {
		// Test 0: No audit store configured, so nothing is read and nothing is committed.
		d := &Dispatcher{store: store, log: zap.NewNop(), ctx: ctx, now: time.Now}
		d.commitSettled([]string{"run_a", "run_b"})
		if got := store.reads.Load(); got != 0 {
			t.Errorf("%d reads with no chain configured, want none", got)
		}
	})

	t.Run("test 1", func(t *testing.T) {
		// Test 1: A chain but nothing settled, so still nothing is read.
		d := &Dispatcher{store: store, audits: audit.NewMemStore(), log: zap.NewNop(),
			ctx: ctx, now: time.Now}
		d.commitSettled(nil)
		d.commitSettled([]string{})
		if got := store.reads.Load(); got != 0 {
			t.Errorf("%d reads for an empty sweep, want none", got)
		}
	})
}

// readCountingStore counts point reads so a test can prove a guard short-circuited before the store
// was touched.
type readCountingStore struct {
	run.Store
	// reads counts calls to Get.
	reads atomic.Int64
}

// Get counts the read and serves the wrapped store.
func (s *readCountingStore) Get(ctx context.Context, id string) (*run.Run, error) {
	s.reads.Add(1)
	return s.Store.Get(ctx, id)
}
