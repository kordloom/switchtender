package retention_test

import (
	"context"
	"testing"
	"time"

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
		if err := store.Save(ctx, &run.Run{ID: id, Status: run.StatusSucceeded, CreatedAt: now.Add(-age)}); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
		if err := store.AppendEvents(ctx, id,
			[]event.Event{{Type: event.TypePlayStart, Time: now.Add(-age)}}); err != nil {
			t.Fatalf("AppendEvents(%s) error = %v", id, err)
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
