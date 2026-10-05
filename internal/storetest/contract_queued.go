package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// testQueuedTimes pins when a waiting run's queue time is recorded: when an approval releases it
// into pending and when the lease sweep puts it back after its worker was lost. A run created
// pending, one moved anywhere but pending, and one no longer pending report none, so a dashboard
// measures each run's wait for a worker from the moment it began rather than from a creation that
// may lie hours behind it on the far side of a hold.
func testQueuedTimes(t *testing.T, store run.Store) {
	ctx := context.Background()
	qt, ok := store.(run.QueueTimes)
	if !ok {
		t.Fatalf("%T does not record queue times", store)
	}
	old := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	runs := []struct {
		ID      string
		Status  run.Status
		Created time.Time
	}{
		// The oldest pending run, so it is the one the claim below takes.
		{"run_requeued", run.StatusPending, old.Add(-time.Minute)},
		{"run_fresh", run.StatusPending, old},
		{"run_released", run.StatusPendingApproval, old},
		{"run_rejected", run.StatusPendingApproval, old},
		{"run_finished", run.StatusPendingApproval, old},
	}
	for _, r := range runs {
		if err := store.Save(ctx, &run.Run{ID: r.ID, Playbook: "p", Status: r.Status,
			CreatedAt: r.Created}); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}
	before := time.Now().Add(-time.Second)
	moves := []struct {
		ID       string
		From, To run.Status
	}{
		{"run_released", run.StatusPendingApproval, run.StatusPending},
		{"run_rejected", run.StatusPendingApproval, run.StatusRejected},
		{"run_finished", run.StatusPendingApproval, run.StatusPending},
		{"run_finished", run.StatusPending, run.StatusSucceeded},
	}
	for _, m := range moves {
		if moved, err := store.TransitionStatus(ctx, m.ID, m.From, m.To); err != nil || !moved {
			t.Fatalf("TransitionStatus(%s, %s to %s) = %v, %v, want moved", m.ID, m.From, m.To,
				moved, err)
		}
	}
	claimed, err := store.Claim(ctx, "worker-gone", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if claimed.ID != "run_requeued" {
		t.Fatalf("Claim() took %s, want the oldest pending run, run_requeued", claimed.ID)
	}
	// A zero age makes the lease stale at once, so the sweep puts the run back in the queue.
	if _, err := store.ReclaimStale(ctx, 0); err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}
	after := time.Now().Add(time.Second)
	got, err := qt.QueuedTimes(ctx)
	if err != nil {
		t.Fatalf("QueuedTimes() error = %v", err)
	}
	tests := []struct {
		ID         string
		WantQueued bool
	}{{ // Test 0: A run created pending has waited since it was created, so it has no entry.
		ID: "run_fresh", WantQueued: false,
	}, { // Test 1: An approval releasing a held run into pending records the moment.
		ID: "run_released", WantQueued: true,
	}, { // Test 2: A rejection is not a move into the queue.
		ID: "run_rejected", WantQueued: false,
	}, { // Test 3: A run that left pending is no longer waiting.
		ID: "run_finished", WantQueued: false,
	}, { // Test 4: The sweep putting back a lost worker's run records a new wait.
		ID: "run_requeued", WantQueued: true,
	}}
	for testNum, test := range tests {
		at, queued := got[test.ID]
		if diff := cmp.Diff(test.WantQueued, queued); diff != "" {
			t.Errorf("test %d: %s queued mismatch (-want +got):\n%s", testNum, test.ID, diff)
			continue
		}
		if queued && (at.Before(before) || at.After(after)) {
			t.Errorf("test %d: %s queued at %v, want between %v and %v", testNum, test.ID, at,
				before, after)
		}
	}
	if diff := cmp.Diff(2, len(got), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("QueuedTimes() size mismatch (-want +got):\n%s", diff)
	}
}
