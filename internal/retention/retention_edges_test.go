package retention_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/retention"
	"github.com/kordloom/switchtender/internal/run"
)

// errSweep is what a spy store returns to stand in for a database that is down or locked during a
// sweep.
var errSweep = errors.New("test: the store refused this purge")

// sweepSpy records every purge a sweep asks for, in order, with the cutoff or count it was asked
// for, and can be told to fail any of the three. It answers everything else from a real in-memory
// store so it satisfies the whole run.Store contract.
type sweepSpy struct {
	// Store answers every method the spy does not record.
	run.Store
	// mu guards the recorded calls.
	mu sync.Mutex
	// calls names each purge in the order the sweep made them.
	calls []string
	// eventsCutoff is the cutoff of the most recent event purge.
	eventsCutoff time.Time
	// runsCutoff is the cutoff of the most recent run purge.
	runsCutoff time.Time
	// keep is the count of the most recent summary trim.
	keep int
	// failEvents, failRuns, and failSummaries make the matching call return an error.
	failEvents, failRuns, failSummaries bool
}

// PurgeEventsBefore records the event cutoff and optionally fails.
func (s *sweepSpy) PurgeEventsBefore(_ context.Context, cutoff time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "events")
	s.eventsCutoff = cutoff
	if s.failEvents {
		return 0, errSweep
	}
	return 1, nil
}

// PurgeRunsBefore records the run cutoff and optionally fails.
func (s *sweepSpy) PurgeRunsBefore(_ context.Context, cutoff time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "runs")
	s.runsCutoff = cutoff
	if s.failRuns {
		return 0, errSweep
	}
	return 1, nil
}

// TrimSummaries records the keep count and optionally fails.
func (s *sweepSpy) TrimSummaries(_ context.Context, keep int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "summaries")
	s.keep = keep
	if s.failSummaries {
		return 0, errSweep
	}
	return 1, nil
}

// snapshot returns a copy of what the spy has recorded so far.
func (s *sweepSpy) snapshot() (calls []string, eventsCutoff, runsCutoff time.Time, keep int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...), s.eventsCutoff, s.runsCutoff, s.keep
}

// countCalls returns how many purges the spy has recorded.
func (s *sweepSpy) countCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// runSweepOnce starts the sweeper, waits for its immediate sweep to reach the spy at least want
// times, then closes it. It fails the test if the sweep never arrives, so a broken sweeper is a
// failure rather than a test that quietly asserts nothing.
func runSweepOnce(t *testing.T, s *retention.Sweeper, spy *sweepSpy, want int) {
	t.Helper()
	s.Start()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && spy.countCalls() < want {
		time.Sleep(time.Millisecond)
	}
	s.Close()
	if got := spy.countCalls(); got < want {
		t.Fatalf("the sweep made %d store calls, want at least %d", got, want)
	}
}

// TestNewSweeperRefusesANilStore pins the constructor's panic. A sweeper built with no store would
// start, tick forever, and report healthy while nothing was ever trimmed, so the database it was
// configured to bound would grow without limit and nobody would learn of it until the disk filled.
func TestNewSweeperRefusesANilStore(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("NewSweeper(nil, ...) returned a sweeper, so a misconfigured server would run " +
				"with retention silently doing nothing")
		}
	}()
	_ = retention.NewSweeper(nil, nil)
}

// TestNewSweeperAcceptsANilLogger pins that a nil logger becomes a no-op rather than a nil
// dereference. The sweep logs on every error path, so a server built without a logger would panic
// inside a background goroutine the first time a purge failed, taking the process down for a
// condition the sweeper is meant to survive.
func TestNewSweeperAcceptsANilLogger(t *testing.T) {
	t.Parallel()
	spy := &sweepSpy{Store: run.NewMemStore(), failEvents: true, failRuns: true, failSummaries: true}
	sweeper := retention.NewSweeper(spy, nil,
		retention.WithRetainEvents(time.Hour),
		retention.WithRetainRuns(2*time.Hour),
		retention.WithRetainHistory(run.MinRetainSummaries))
	runSweepOnce(t, sweeper, spy, 3)
}

// TestSweepOrdersEventsThenRunsThenSummaries pins the sweep's order, which the package documents as
// deliberate. Events are trimmed first so the run rows deleted next are already light, and the
// summaries come last because deleting runs does not touch them: they outlive their runs on purpose
// and the count bound is the only thing that ever removes one.
func TestSweepOrdersEventsThenRunsThenSummaries(t *testing.T) {
	t.Parallel()
	spy := &sweepSpy{Store: run.NewMemStore()}
	sweeper := retention.NewSweeper(spy, nil,
		retention.WithRetainEvents(24*time.Hour),
		retention.WithRetainRuns(72*time.Hour),
		retention.WithRetainHistory(run.MinRetainSummaries))
	runSweepOnce(t, sweeper, spy, 3)

	calls, _, _, _ := spy.snapshot()
	want := []string{"events", "runs", "summaries"}
	if diff := cmp.Diff(want, calls[:min(len(calls), 3)], cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("sweep order mismatch (-want +got):\n%s", diff)
	}
}

// TestSweepContinuesAfterAStoreError pins that one failing purge does not abandon the rest of the
// sweep. A locked table or a transient database error on the event purge must not leave run
// deletion and the summary bound unattempted, because the two tables with no other bound are
// exactly the ones swept last.
func TestSweepContinuesAfterAStoreError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which purge fails.
		Name string
		// FailEvents, FailRuns, and FailSummaries choose the failing call.
		FailEvents, FailRuns, FailSummaries bool
	}{{ // Test 0: The first step fails.
		Name: "events fail", FailEvents: true,
	}, { // Test 1: The middle step fails.
		Name: "runs fail", FailRuns: true,
	}, { // Test 2: The last step fails.
		Name: "summaries fail", FailSummaries: true,
	}, { // Test 3: Every step fails, and the sweep still attempts all three.
		Name: "all fail", FailEvents: true, FailRuns: true, FailSummaries: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			spy := &sweepSpy{
				Store: run.NewMemStore(), failEvents: test.FailEvents,
				failRuns: test.FailRuns, failSummaries: test.FailSummaries,
			}
			sweeper := retention.NewSweeper(spy, nil,
				retention.WithRetainEvents(time.Hour),
				retention.WithRetainRuns(2*time.Hour),
				retention.WithRetainHistory(run.MinRetainSummaries))
			runSweepOnce(t, sweeper, spy, 3)

			calls, _, _, _ := spy.snapshot()
			want := []string{"events", "runs", "summaries"}
			if diff := cmp.Diff(want, calls[:min(len(calls), 3)], cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s: the sweep stopped early (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestSweepCutoffsAreTheConfiguredWindowsFromOneInstant pins the arithmetic that decides what is
// deleted. Both cutoffs are taken from a single reading of the clock, so the gap between them is
// exactly the difference between the two windows. If the run cutoff were ever computed from a later
// instant than the event cutoff, runs would be deleted on a boundary the operator did not
// configure, and deletion is the one operation here that cannot be undone.
func TestSweepCutoffsAreTheConfiguredWindowsFromOneInstant(t *testing.T) {
	t.Parallel()
	const retainEvents = 30 * 24 * time.Hour
	const retainRuns = 90 * 24 * time.Hour

	spy := &sweepSpy{Store: run.NewMemStore()}
	sweeper := retention.NewSweeper(spy, nil,
		retention.WithRetainEvents(retainEvents), retention.WithRetainRuns(retainRuns))
	before := time.Now()
	runSweepOnce(t, sweeper, spy, 2)
	after := time.Now()

	_, eventsCutoff, runsCutoff, _ := spy.snapshot()
	// The two cutoffs come from the same now, so their difference is exact, not approximate.
	if got := eventsCutoff.Sub(runsCutoff); got != retainRuns-retainEvents {
		t.Errorf("the gap between the two cutoffs is %v, want exactly %v, so the sweep read the "+
			"clock twice", got, retainRuns-retainEvents)
	}
	// Each cutoff is its window behind an instant inside the sweep.
	if eventsCutoff.Before(before.Add(-retainEvents)) || eventsCutoff.After(after.Add(-retainEvents)) {
		t.Errorf("event cutoff %v is not %v behind the sweep instant", eventsCutoff, retainEvents)
	}
	if runsCutoff.Before(before.Add(-retainRuns)) || runsCutoff.After(after.Add(-retainRuns)) {
		t.Errorf("run cutoff %v is not %v behind the sweep instant", runsCutoff, retainRuns)
	}
}

// TestOnlyConfiguredWindowsAct pins that an unset window means the sweep never touches that table.
// Each of the three is separately configurable, and an operator who set only an event window has
// not consented to runs being deleted or a host's outcome history being trimmed. A zero that leaked
// through as a window would delete everything, since every run is older than the current instant.
func TestOnlyConfiguredWindowsAct(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which windows are configured.
		Name string
		// RetainEvents, RetainRuns, and RetainHistory are the configured windows.
		RetainEvents, RetainRuns time.Duration
		RetainHistory            int
		// WantCalls are the purges that must happen, in order.
		WantCalls []string
		// WantEnabled is what Enabled must report.
		WantEnabled bool
	}{{ // Test 0: Nothing configured, so nothing is ever asked of the store.
		Name: "none", WantCalls: nil, WantEnabled: false,
	}, { // Test 1: Events only, so runs survive and summaries are untouched.
		Name: "events only", RetainEvents: time.Hour, WantCalls: []string{"events"}, WantEnabled: true,
	}, { // Test 2: Runs only.
		Name: "runs only", RetainRuns: time.Hour, WantCalls: []string{"runs"}, WantEnabled: true,
	}, { // Test 3: History only.
		Name: "history only", RetainHistory: run.MinRetainSummaries,
		WantCalls: []string{"summaries"}, WantEnabled: true,
	}, { // Test 4: A negative window is not a window, so it must act like unset rather than as an
		// instant cutoff that deletes the whole table.
		Name: "negative windows", RetainEvents: -time.Hour, RetainRuns: -time.Hour,
		RetainHistory: -5, WantCalls: nil, WantEnabled: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			spy := &sweepSpy{Store: run.NewMemStore()}
			sweeper := retention.NewSweeper(spy, nil,
				retention.WithRetainEvents(test.RetainEvents),
				retention.WithRetainRuns(test.RetainRuns),
				retention.WithRetainHistory(test.RetainHistory))
			if got := sweeper.Enabled(); got != test.WantEnabled {
				t.Errorf("%s: Enabled() = %v, want %v", test.Name, got, test.WantEnabled)
			}
			sweeper.Start()
			if len(test.WantCalls) > 0 {
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) && spy.countCalls() < len(test.WantCalls) {
					time.Sleep(time.Millisecond)
				}
			} else {
				// Nothing should ever arrive, so give a sweep that wrongly ran time to show itself.
				time.Sleep(50 * time.Millisecond)
			}
			sweeper.Close()

			calls, _, _, _ := spy.snapshot()
			if diff := cmp.Diff(test.WantCalls, calls, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s: purges mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestNegativeHistoryNeverReachesTheStore pins the one configuration that would destroy data. The
// store treats a keep below one as one, so a negative count arriving there would trim every host's
// and every task's history down to a single row, permanently, on the first tick. The sweeper's own
// guard is what stops that, and it is checked here rather than at the store because this is where
// the count is chosen.
func TestNegativeHistoryNeverReachesTheStore(t *testing.T) {
	t.Parallel()
	for testNum, configured := range []int{-1, -500, -run.MinRetainSummaries} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			spy := &sweepSpy{Store: run.NewMemStore()}
			sweeper := retention.NewSweeper(spy, nil, retention.WithRetainHistory(configured))
			if sweeper.Enabled() {
				t.Errorf("a history count of %d enabled the sweeper", configured)
			}
			sweeper.Start()
			time.Sleep(50 * time.Millisecond)
			sweeper.Close()
			if calls, _, _, keep := spy.snapshot(); len(calls) != 0 {
				t.Errorf("a history count of %d reached the store as keep=%d, which would trim "+
					"every host's history to one row", configured, keep)
			}
		})
	}
}

// TestNonPositiveIntervalFallsBackToTheDefault pins the guard that keeps a bad interval from
// crashing the server. A ticker refuses a duration of zero or less by panicking, and this ticker is
// created inside a background goroutine, so an operator configuring an interval of zero would take
// the whole process down rather than get a default.
func TestNonPositiveIntervalFallsBackToTheDefault(t *testing.T) {
	t.Parallel()
	for testNum, interval := range []time.Duration{0, -time.Second, -time.Hour} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			spy := &sweepSpy{Store: run.NewMemStore()}
			sweeper := retention.NewSweeper(spy, nil,
				retention.WithRetainRuns(time.Hour), retention.WithInterval(interval))
			runSweepOnce(t, sweeper, spy, 1)
			// One immediate sweep, and then the default hour, so nothing further arrives.
			if got := spy.countCalls(); got != 1 {
				t.Errorf("interval %v produced %d sweeps, want the single immediate one followed "+
					"by the default interval", interval, got)
			}
		})
	}
}

// TestPositiveIntervalKeepsSweeping pins that the loop actually ticks rather than sweeping once and
// going quiet. A sweeper that ran only at startup would bound the database on the day it was
// deployed and never again, which reads as configured retention while the tables grow.
func TestPositiveIntervalKeepsSweeping(t *testing.T) {
	t.Parallel()
	spy := &sweepSpy{Store: run.NewMemStore()}
	sweeper := retention.NewSweeper(spy, nil,
		retention.WithRetainRuns(time.Hour), retention.WithInterval(2*time.Millisecond))
	sweeper.Start()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && spy.countCalls() < 3 {
		time.Sleep(time.Millisecond)
	}
	sweeper.Close()
	if got := spy.countCalls(); got < 3 {
		t.Errorf("the sweeper made %d sweeps on a 2ms interval, want it to keep ticking", got)
	}
}

// TestCloseStopsTheSweepAndIsSafeToRepeat pins that Close ends the loop and can be called more than
// once, and that closing a sweeper that was never started returns rather than blocking. Shutdown
// runs these paths in whatever order a failing startup leaves behind, and a Close that hung would
// hold the process open past its shutdown deadline.
func TestCloseStopsTheSweepAndIsSafeToRepeat(t *testing.T) {
	t.Parallel()

	// Test 0: Close without Start returns immediately.
	never := retention.NewSweeper(run.NewMemStore(), nil, retention.WithRetainRuns(time.Hour))
	never.Close()
	never.Close()

	// Test 1: Close stops the ticking loop, and no sweep arrives afterward.
	spy := &sweepSpy{Store: run.NewMemStore()}
	sweeper := retention.NewSweeper(spy, nil,
		retention.WithRetainRuns(time.Hour), retention.WithInterval(2*time.Millisecond))
	sweeper.Start()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && spy.countCalls() < 2 {
		time.Sleep(time.Millisecond)
	}
	sweeper.Close()
	sweeper.Close()
	settled := spy.countCalls()
	time.Sleep(50 * time.Millisecond)
	if got := spy.countCalls(); got != settled {
		t.Errorf("the store was swept %d more times after Close returned", got-settled)
	}
}

// TestSweepDeletesOnlyOutsideTheWindow drives a real store through the sweeper and pins both sides
// of the run boundary: a terminal run one nanosecond older than the window is deleted, one inside
// it is kept, and a run that has not finished is kept however old it is. Deleting a run that is
// still going would destroy the record of work in progress, and no window makes that correct.
func TestSweepDeletesOnlyOutsideTheWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	const window = 24 * time.Hour
	// The sweep stamps its cutoff from the wall clock a moment after these are seeded, so ages are
	// taken from now and the inside case is given a margin far wider than that gap.
	now := time.Now()

	seed := func(id string, age time.Duration, status run.Status) {
		t.Helper()
		if err := store.Save(ctx, &run.Run{
			ID: id, Status: run.StatusRunning, CreatedAt: now.Add(-age),
		}); err != nil {
			t.Fatalf("Save(%s): %v", id, err)
		}
		if err := store.AppendEvents(ctx, id,
			[]event.Event{{Type: event.TypePlayStart, Time: now.Add(-age)}}); err != nil {
			t.Fatalf("AppendEvents(%s): %v", id, err)
		}
		if status != run.StatusRunning {
			if err := store.Save(ctx, &run.Run{
				ID: id, Status: status, CreatedAt: now.Add(-age),
			}); err != nil {
				t.Fatalf("Save(%s) terminal: %v", id, err)
			}
		}
	}
	seed("just-outside", window+time.Nanosecond, run.StatusSucceeded)
	seed("well-inside", window-time.Hour, run.StatusSucceeded)
	seed("ancient-running", 400*24*time.Hour, run.StatusRunning)

	sweeper := retention.NewSweeper(store, nil, retention.WithRetainRuns(window))
	sweeper.Start()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := store.Get(ctx, "just-outside"); err != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	sweeper.Close()

	if _, err := store.Get(ctx, "just-outside"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("a terminal run past the window survived: err = %v", err)
	}
	if _, err := store.Get(ctx, "well-inside"); err != nil {
		t.Errorf("a terminal run inside the window was deleted: %v", err)
	}
	if _, err := store.Get(ctx, "ancient-running"); err != nil {
		t.Errorf("a run that has not finished was deleted despite its age: %v", err)
	}
}

// TestSweeperSurvivesConcurrentUse pins that the sweeper's exported surface is safe to touch from
// several goroutines at once, which is what a shutdown racing a health check does. Under the race
// detector this proves the sweep loop shares nothing unguarded with its callers.
func TestSweeperSurvivesConcurrentUse(t *testing.T) {
	t.Parallel()
	spy := &sweepSpy{Store: run.NewMemStore()}
	sweeper := retention.NewSweeper(spy, nil,
		retention.WithRetainEvents(time.Hour),
		retention.WithRetainRuns(2*time.Hour),
		retention.WithRetainHistory(run.MinRetainSummaries),
		retention.WithInterval(time.Millisecond))
	sweeper.Start()

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				_ = sweeper.Enabled()
			}
		}()
	}
	wg.Wait()
	sweeper.Close()
}
