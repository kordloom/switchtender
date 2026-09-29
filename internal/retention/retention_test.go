package retention_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/retention"
	"github.com/kordloom/switchtender/internal/run"
)

// TestSweepTrimsThenDeletes verifies a sweep trims events at the shorter window and deletes runs at
// the longer one, leaving recent runs alone.
func TestSweepTrimsThenDeletes(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	ctx := context.Background()
	// The sweeper's immediate sweep stamps its cutoffs from the wall clock, so seed ages relative
	// to the current time rather than a fixed instant.
	now := time.Now()

	// A run 100 days old, one 40 days old, one an hour old, all terminal with events.
	ages := map[string]time.Duration{
		"ancient": 100 * 24 * time.Hour,
		"middle":  40 * 24 * time.Hour,
		"fresh":   time.Hour,
	}
	for id, age := range ages {
		// Events land while the run is running, then it finalizes, since the store fences event writes
		// to a terminal run.
		if err := store.Save(ctx, &run.Run{ID: id, Status: run.StatusRunning, CreatedAt: now.Add(-age)}); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
		if err := store.AppendEvents(ctx, id,
			[]event.Event{{Type: event.TypePlayStart, Time: now.Add(-age)}}); err != nil {
			t.Fatalf("AppendEvents(%s) error = %v", id, err)
		}
		if err := store.Save(ctx, &run.Run{ID: id, Status: run.StatusSucceeded, CreatedAt: now.Add(-age)}); err != nil {
			t.Fatalf("Save(%s) finalize error = %v", id, err)
		}
	}

	sweeper := retention.NewSweeper(store, nil,
		retention.WithRetainEvents(30*24*time.Hour), retention.WithRetainRuns(90*24*time.Hour))

	// The sweep goroutine runs sweep; drive it deterministically by starting and closing, but the
	// timing is nondeterministic, so exercise the exported behavior through Start with a settle.
	sweeper.Start()
	// Give the immediate sweep time to run, then stop.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := store.Get(ctx, "ancient"); err != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	sweeper.Close()

	// Ancient run deleted, its 100-day age past the 90-day run window.
	if _, err := store.Get(ctx, "ancient"); err == nil {
		t.Error("ancient run survived past the run retention window")
	}
	// Middle run kept, but its events trimmed since 40 days is past the 30-day event window.
	if _, err := store.Get(ctx, "middle"); err != nil {
		t.Errorf("middle run deleted despite being within the run window: %v", err)
	}
	if evs, _ := store.Events(ctx, "middle"); len(evs) != 0 {
		t.Errorf("middle events = %v, want trimmed", evs)
	}
	// Fresh run and its events untouched.
	if evs, _ := store.Events(ctx, "fresh"); len(evs) != 1 {
		t.Errorf("fresh events = %v, want kept", evs)
	}
}

// TestDisabledSweeperDoesNothing verifies a sweeper with no windows reports disabled and never
// touches the store.
func TestDisabledSweeperDoesNothing(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	ctx := context.Background()
	if err := store.Save(ctx, &run.Run{
		ID: "old", Status: run.StatusSucceeded,
		CreatedAt: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	sweeper := retention.NewSweeper(store, nil)
	if sweeper.Enabled() {
		t.Error("Enabled() = true with no windows configured")
	}
	sweeper.Start()
	sweeper.Close()
	if _, err := store.Get(ctx, "old"); err != nil {
		t.Errorf("disabled sweeper removed a run: %v", err)
	}
}

// trimSpy records what a sweep asks TrimSummaries for, so a test can assert the count that reached
// the store rather than the count that was configured.
type trimSpy struct {
	// Store answers every method the spy does not record.
	run.Store
	// calls counts how many times a sweep called TrimSummaries.
	calls atomic.Int64
	// keep holds the count of the most recent call.
	keep atomic.Int64
}

// TrimSummaries records the requested keep and reports nothing deleted.
func (s *trimSpy) TrimSummaries(_ context.Context, keep int) (int, error) {
	s.keep.Store(int64(keep))
	s.calls.Add(1)
	return 0, nil
}

// TestRetainHistoryFloor verifies the configured summary count that reaches the store.
//
// The fleet views let a caller ask for a window as deep as run.MaxHostHistory, and the summaries
// are the only record a host's outcome leaves once its run is deleted. A trim shallower than that
// window would answer a legal request with a truncated history and report a host as quieter than it
// was, so a smaller setting is raised rather than obeyed. Zero still means never trim, since
// keeping every summary forever is the behavior an operator gets by not configuring this at all.
func TestRetainHistoryFloor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Configured is the count passed to WithRetainHistory.
		Configured int
		// WantKeep is the count the store must be asked to trim to. Zero means no call at all.
		WantKeep int
	}{
		{Configured: 0, WantKeep: 0},                                               // Test 0: Unset never trims.
		{Configured: 1, WantKeep: run.MinRetainSummaries},                          // Test 1: One is raised to the floor.
		{Configured: run.MinRetainSummaries - 1, WantKeep: run.MinRetainSummaries}, // Test 2: Just under is raised.
		{Configured: run.MinRetainSummaries, WantKeep: run.MinRetainSummaries},     // Test 3: The floor itself stands.
		{Configured: 5000, WantKeep: 5000},                                         // Test 4: A generous count is left alone.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			spy := &trimSpy{Store: run.NewMemStore()}
			sweeper := retention.NewSweeper(spy, nil, retention.WithRetainHistory(test.Configured))
			if got := sweeper.Enabled(); got != (test.WantKeep > 0) {
				t.Errorf("Enabled() = %v, want %v", got, test.WantKeep > 0)
			}

			sweeper.Start()
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) && spy.calls.Load() == 0 {
				time.Sleep(time.Millisecond)
			}
			sweeper.Close()

			if test.WantKeep == 0 {
				if n := spy.calls.Load(); n != 0 {
					t.Fatalf("TrimSummaries called %d times with trimming disabled", n)
				}
				return
			}
			if spy.calls.Load() == 0 {
				t.Fatal("the sweep never called TrimSummaries")
			}
			if diff := cmp.Diff(int64(test.WantKeep), spy.keep.Load()); diff != "" {
				t.Errorf("keep reaching the store mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
