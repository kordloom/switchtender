package run_test

import (
	"context"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// TestASweptCancelIsSettledNotRequeued is the in-memory store's half of the rule the SQL stores keep.
//
// Cancelling a claimed run is cooperative: the flag is set for its holder to read. A holder that died
// before starting the run leaves nobody to read it, and a claim will not take a cancel-flagged run, so
// requeuing it left the run pending, unclaimable, and past the reach of every sweep, reported as
// canceling for as long as anyone looked. The sweep settles it instead, which is what the person asked
// for and what the run, never having started, can honestly be recorded as.
//
// It is asserted per store because each one implements the sweep separately, and a rule kept in two of
// three is the kind of gap a deployment discovers rather than a test.
func TestASweptCancelIsSettledNotRequeued(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()

	stale := time.Now().Add(-10 * time.Minute)
	canceled := &run.Run{
		ID: "run_swept_cancel", Playbook: "site.yml", Status: run.StatusPending, CreatedAt: stale,
		ClaimedBy: "worker-that-died", ClaimedAt: &stale, CancelRequested: true,
	}
	plain := &run.Run{
		ID: "run_swept_plain", Playbook: "site.yml", Status: run.StatusPending, CreatedAt: stale,
		ClaimedBy: "worker-that-died", ClaimedAt: &stale,
	}
	for _, r := range []*run.Run{canceled, plain} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}

	if _, err := store.ReclaimStale(ctx, 30*time.Second); err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}

	got, err := store.Get(ctx, canceled.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusCanceled {
		t.Errorf("a swept cancel is %q, want canceled: a cancel-flagged pending run can never be "+
			"claimed and no sweep settles a pending one, so it would never finish", got.Status)
	}
	if got.EndedAt == nil {
		t.Error("the settled run records no end, so it has no duration and no place in history")
	}

	// A stale claim nobody canceled still goes back in the queue, which is what the sweep is for.
	back, err := store.Get(ctx, plain.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if back.Status != run.StatusPending || back.ClaimedBy != "" {
		t.Errorf("an uncanceled stale claim came back as %q held by %q, want pending and unheld",
			back.Status, back.ClaimedBy)
	}
}
