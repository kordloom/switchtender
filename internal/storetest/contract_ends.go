package storetest

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// testEndLedger verifies every path that moves a top-level run to a terminal status leaves its end
// owed, whichever write makes the move, while a run that has not ended, a run stored already
// finished, and a child of a split or pipeline owe nothing, and that a settled end stays settled.
func testEndLedger(t *testing.T, store run.Store) {
	ledger, ok := store.(run.EndLedger)
	if !ok {
		t.Fatalf("%T keeps no ledger of owed run ends, so a run whose process stopped after it "+
			"ended is never announced", store)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	stale := now.Add(-2 * time.Hour)
	parent := "end_parent"
	for _, r := range []*run.Run{
		{ID: "end_finalized", Status: run.StatusRunning, ClaimedBy: "w1", ClaimedAt: &now,
			StartedAt: &now},
		{ID: "end_settled", Status: run.StatusPendingApproval},
		{ID: "end_canceled", Status: run.StatusPending},
		{ID: "end_reclaimed", Status: run.StatusRunning, ClaimedBy: "gone", ClaimedAt: &stale,
			StartedAt: &stale},
		{ID: "end_rejected", Status: run.StatusPendingApproval},
		{ID: "end_saved", Status: run.StatusPending},
		{ID: "end_live", Status: run.StatusRunning, ClaimedBy: "w1", ClaimedAt: &now,
			StartedAt: &now},
		{ID: "end_imported", Status: run.StatusSucceeded, EndedAt: &now},
		{ID: parent, Status: run.StatusSucceeded, EndedAt: &now},
		{ID: "end_child", Status: run.StatusPending, ParentID: &parent},
	} {
		r.Playbook, r.CreatedAt = "site.yml", stale
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}
	moves := []struct {
		Name string
		Move func() (bool, error)
	}{{
		Name: "finalize running",
		Move: func() (bool, error) {
			return store.FinalizeRunning(ctx, "end_finalized", run.Finalization{
				Status: run.StatusSucceeded, EndedAt: now, Owner: "w1"})
		},
	}, {
		Name: "settle held",
		Move: func() (bool, error) {
			return store.SettleHeld(ctx, "end_settled", run.Finalization{
				Status: run.StatusFailed, Error: "timed out", EndedAt: now})
		},
	}, {
		Name: "cancel pending",
		Move: func() (bool, error) { return store.CancelPending(ctx, "end_canceled") },
	}, {
		Name: "reclaim stale",
		Move: func() (bool, error) {
			n, err := store.ReclaimStale(ctx, time.Minute)
			return n > 0, err
		},
	}, {
		Name: "transition status",
		Move: func() (bool, error) {
			return store.TransitionStatus(ctx, "end_rejected", run.StatusPendingApproval,
				run.StatusRejected)
		},
	}, {
		Name: "whole-row save",
		Move: func() (bool, error) {
			r, err := store.Get(ctx, "end_saved")
			if err != nil {
				return false, err
			}
			r.Status, r.EndedAt = run.StatusFailed, &now
			return true, store.Save(ctx, r)
		},
	}, {
		Name: "cancel a child",
		Move: func() (bool, error) { return store.CancelPending(ctx, "end_child") },
	}}
	for _, m := range moves {
		if moved, err := m.Move(); err != nil || !moved {
			t.Fatalf("%s: moved = %v, error = %v, want the run ended", m.Name, moved, err)
		}
	}

	owed, err := ledger.OwedEnds(ctx, 0, 0)
	if err != nil {
		t.Fatalf("OwedEnds() error = %v", err)
	}
	want := []string{"end_canceled", "end_finalized", "end_reclaimed", "end_rejected",
		"end_saved", "end_settled"}
	if diff := cmp.Diff(want, owed, cmpopts.SortSlices(func(a, b string) bool {
		return a < b
	})); diff != "" {
		t.Errorf("owed ends mismatch (-want +got):\n%s", diff)
	}
	// The database stores keep the two ledgers with two triggers on the same status write, so the
	// runs owing their outcome to the chain are exactly the runs owing their end, on every path.
	// A trigger that stopped firing, or one that replaced the other under a shared name, leaves
	// the two sets apart.
	outcomes, err := store.OwedOutcomes(ctx, 0, 100)
	if err != nil {
		t.Fatalf("OwedOutcomes() error = %v", err)
	}
	if diff := cmp.Diff(want, outcomes, cmpopts.SortSlices(func(a, b string) bool {
		return a < b
	})); diff != "" {
		t.Errorf("owed outcomes differ from owed ends (-want +got):\n%s", diff)
	}
	if limited, err := ledger.OwedEnds(ctx, 0, 2); err != nil || len(limited) != 2 {
		t.Errorf("OwedEnds() with a limit of 2 = %v, %v", limited, err)
	}
	if early, err := ledger.OwedEnds(ctx, time.Hour, 0); err != nil || len(early) != 0 {
		t.Errorf("OwedEnds() an hour of grace after the ends = %v, %v, want none yet", early, err)
	}

	for _, id := range []string{"end_finalized", "end_finalized", "end_ghost"} {
		if err := ledger.SettleEnd(ctx, id); err != nil {
			t.Fatalf("SettleEnd(%s) error = %v", id, err)
		}
	}
	// A settled run saved again, still finished, owes nothing new.
	again, err := store.Get(ctx, "end_finalized")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	again.Warning = "noted after the end"
	if err := store.Save(ctx, again); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	owed, err = ledger.OwedEnds(ctx, 0, 0)
	if err != nil {
		t.Fatalf("OwedEnds() after settling error = %v", err)
	}
	sort.Strings(owed)
	wantAfter := []string{"end_canceled", "end_reclaimed", "end_rejected", "end_saved",
		"end_settled"}
	if diff := cmp.Diff(wantAfter, owed); diff != "" {
		t.Errorf("owed ends after settling end_finalized and saving it again mismatch "+
			"(-want +got):\n%s", diff)
	}
}

// testReclaimAttributesAStaleCancel verifies the lease sweep names a top-level run it ends as
// canceled, one whose holder died after somebody asked to cancel it, among the runs it settled, so
// its outcome is committed and its end announced like every other run the sweep ends, while a
// child it cancels the same way is left to its parent.
func testReclaimAttributesAStaleCancel(t *testing.T, store run.Store) {
	reporter, ok := store.(settledReporter)
	if !ok {
		t.Fatalf("%T does not implement ReclaimStaleSettled", store)
	}
	ctx := context.Background()
	stale := time.Now().Add(-2 * time.Hour)
	parent := "cancel_parent"
	for _, r := range []*run.Run{
		{ID: "cancel_top", Status: run.StatusPending, ClaimedBy: "gone", ClaimedAt: &stale},
		{ID: parent, Status: run.StatusRunning, ClaimedBy: "live", ClaimedAt: &stale,
			Kind: run.KindSplit},
		{ID: "cancel_child", Status: run.StatusPending, ClaimedBy: "gone", ClaimedAt: &stale,
			ParentID: &parent},
	} {
		r.Playbook, r.CreatedAt = "site.yml", stale
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
		if err := store.RequestCancel(ctx, r.ID); err != nil {
			t.Fatalf("RequestCancel(%s) error = %v", r.ID, err)
		}
	}
	_, settled, err := reporter.ReclaimStaleSettled(ctx, time.Minute)
	if err != nil {
		t.Fatalf("ReclaimStaleSettled() error = %v", err)
	}
	got := map[string]bool{}
	for _, id := range settled {
		got[id] = true
	}
	if !got["cancel_top"] || got["cancel_child"] {
		t.Errorf("settled = %v, want cancel_top named and cancel_child left to its parent", settled)
	}
	for _, id := range []string{"cancel_top", "cancel_child"} {
		r, err := store.Get(ctx, id)
		if err != nil || r.Status != run.StatusCanceled {
			t.Errorf("%s after the sweep = %v, %v, want canceled", id, r, err)
		}
	}
}
