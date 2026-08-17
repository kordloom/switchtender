package sqlitestore_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// TestASweptCancelDoesNotStrandTheRun covers a state nothing could get a run out of.
//
// Cancelling a run that a worker holds is cooperative: the flag is set and the executor stops when it
// reads it. If that worker dies before it ever saved the run running, the run is still pending with a
// dead holder, so the janitor requeues it by clearing the lease. It kept the cancel flag, and a claim
// will not take a cancel-flagged run, so the run sat pending forever: unclaimable because of the flag,
// never terminal because nothing sweeps a pending run, and reported as canceling for as long as anyone
// cared to look. No retry, no sweep, and no timeout resolved it. Only a person cancelling a second time
// ended it, which an automation that issued its one cancel would never do.
//
// The approval path was fixed for this exact black hole. The requeue recreated it.
func TestASweptCancelDoesNotStrandTheRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := db.Runs()

	// A run a dead worker claimed and never started, which a person then asked to cancel.
	stale := time.Now().Add(-10 * time.Minute)
	r := &run.Run{
		ID: "run_swept_cancel", Playbook: "site.yml", Status: run.StatusPending, CreatedAt: stale,
		ClaimedBy: "worker-that-died", ClaimedAt: &stale, CancelRequested: true,
	}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if _, err := store.ReclaimStale(ctx, 30*time.Second); err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}

	got, err := store.Get(ctx, r.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !got.Status.Terminal() {
		t.Fatalf("the run is %q with cancel_requested=%v and holder %q: a claim will not take a "+
			"cancel-flagged run and no sweep settles a pending one, so it can never finish",
			got.Status, got.CancelRequested, got.ClaimedBy)
	}
	if got.Status != run.StatusCanceled {
		t.Errorf("status = %q, want canceled: a person asked for it and the run never started",
			got.Status)
	}
	if got.EndedAt == nil {
		t.Error("the settled run records no end, so it has no duration and no place in history")
	}

	// A stale claim with no cancel asked for is still requeued to run, which is what the sweep is for.
	live := &run.Run{
		ID: "run_swept_plain", Playbook: "site.yml", Status: run.StatusPending, CreatedAt: stale,
		ClaimedBy: "worker-that-died", ClaimedAt: &stale,
	}
	if err := store.Save(ctx, live); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := store.ReclaimStale(ctx, 30*time.Second); err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}
	back, err := store.Get(ctx, live.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if back.Status != run.StatusPending || back.ClaimedBy != "" {
		t.Errorf("an uncanceled stale claim came back as %q held by %q, want pending and unheld",
			back.Status, back.ClaimedBy)
	}
}
