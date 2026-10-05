package storetest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// testOutcomeOwed verifies a top-level run comes to owe its outcome to the chain in the write that
// makes it terminal, whichever write that is, and owes nothing once the outcome is settled. A run
// stored already finished and a child whose outcome is rolled into its parent never owe one. The
// janitor commits exactly what this reports, so a run missing here is an outcome lost for good when
// its commit fails, and a run listed wrongly is an outcome committed for nothing that finished.
func testOutcomeOwed(t *testing.T, store run.Store) {
	t.Helper()
	ctx := context.Background()
	created := time.Now().Add(-time.Hour)
	parent := "run_owed_parent"
	for _, r := range []*run.Run{
		{ID: "run_owed_finalized", Playbook: "p.yml", Status: run.StatusRunning, ClaimedBy: "w"},
		{ID: "run_owed_canceled", Playbook: "p.yml", Status: run.StatusPending},
		{ID: "run_owed_rejected", Playbook: "p.yml", Status: run.StatusPendingApproval},
		{ID: "run_owed_stored", Playbook: "p.yml", Status: run.StatusSucceeded, EndedAt: &created},
		{ID: parent, Playbook: "p.yml", Status: run.StatusRunning, Kind: run.KindPipeline},
		{ID: "run_owed_child", Playbook: "p.yml", Status: run.StatusRunning, ClaimedBy: "w",
			ParentID: &parent},
	} {
		r.CreatedAt = created
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}
	ended := time.Now()
	fin := run.Finalization{Status: run.StatusSucceeded, EndedAt: ended}
	for _, id := range []string{"run_owed_finalized", "run_owed_child"} {
		if moved, err := store.FinalizeRunning(ctx, id, fin); err != nil || !moved {
			t.Fatalf("FinalizeRunning(%s) = %t, %v", id, moved, err)
		}
	}
	if done, err := store.CancelPending(ctx, "run_owed_canceled"); err != nil || !done {
		t.Fatalf("CancelPending() = %t, %v", done, err)
	}
	if ok, err := store.TransitionStatus(ctx, "run_owed_rejected", run.StatusPendingApproval,
		run.StatusRejected); err != nil || !ok {
		t.Fatalf("TransitionStatus() = %t, %v", ok, err)
	}

	owed, err := store.OwedOutcomes(ctx, 0, 100)
	if err != nil {
		t.Fatalf("OwedOutcomes() error = %v", err)
	}
	want := []string{"run_owed_canceled", "run_owed_finalized", "run_owed_rejected"}
	if diff := cmp.Diff(want, owed, cmpopts.SortSlices(func(a, b string) bool { return a < b }),
		cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("OwedOutcomes() mismatch (-want +got):\n%s", diff)
	}
	if young, err := store.OwedOutcomes(ctx, time.Hour, 100); err != nil || len(young) != 0 {
		t.Errorf("OwedOutcomes(an hour) = %v, %v, want none owed that long", young, err)
	}
	if one, err := store.OwedOutcomes(ctx, 0, 1); err != nil || len(one) != 1 {
		t.Errorf("OwedOutcomes(limit 1) = %v, %v, want exactly one", one, err)
	}

	if err := store.SettleOutcome(ctx, "run_owed_finalized"); err != nil {
		t.Fatalf("SettleOutcome() error = %v", err)
	}
	tests := []struct {
		ID       string
		WantOwed bool
	}{
		{ID: "run_owed_finalized", WantOwed: false}, // Test 0: Settled.
		{ID: "run_owed_canceled", WantOwed: true},   // Test 1: Canceled while waiting.
		{ID: "run_owed_rejected", WantOwed: true},   // Test 2: Rejected by an approver.
		{ID: "run_owed_stored", WantOwed: false},    // Test 3: Stored already finished.
		{ID: "run_owed_child", WantOwed: false},     // Test 4: A child, rolled into its parent.
		{ID: parent, WantOwed: false},               // Test 5: Still running.
		{ID: "run_owed_missing", WantOwed: false},   // Test 6: No such run.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			got, err := store.OutcomeOwed(ctx, test.ID)
			if err != nil || got != test.WantOwed {
				t.Errorf("OutcomeOwed(%s) = %t, %v, want %t", test.ID, got, err, test.WantOwed)
			}
		})
	}
	// Saving the finished run again, as a re-save of its claim stamp does, owes nothing new.
	again, err := store.Get(ctx, "run_owed_finalized")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if err := store.Save(ctx, again); err != nil {
		t.Fatalf("Save() of the finished run error = %v", err)
	}
	if owes, err := store.OutcomeOwed(ctx, "run_owed_finalized"); err != nil || owes {
		t.Errorf("OutcomeOwed() after a re-save of a settled run = %t, %v, want false", owes, err)
	}
}
