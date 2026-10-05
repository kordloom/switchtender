package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

// decisionClaimContract runs the store behavior a decision's claim depends on: one decision claims
// a held run, nothing but that decision moves it out of pending_approval, and settling it is a
// compare-and-set on the claim. It also pins that a parked workflow is no held run. Its cases run
// one after another, since a database store's newStore empties the database the others share.
func decisionClaimContract(t *testing.T, newStore func() run.Store) {
	t.Helper()
	t.Run("a decision claims a held run once", func(t *testing.T) {
		testClaimDecisionOnce(t, newStore)
	})
	t.Run("a claimed run is moved by its decision alone", func(t *testing.T) {
		testClaimedRunIsFenced(t, newStore)
	})
	t.Run("a decision settles the run it claimed", func(t *testing.T) {
		testSettleDecision(t, newStore)
	})
	t.Run("a parked workflow is no held run", func(t *testing.T) {
		testParkedIsNoHeldRun(t, newStore())
	})
}

// claimHeld saves a run held for approval under id, shaped by mod when it is not nil.
func claimHeld(t *testing.T, store run.Store, id string, mod func(*run.Run)) {
	t.Helper()
	r := &run.Run{ID: id, Playbook: "site.yml", Status: run.StatusPendingApproval,
		CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	if mod != nil {
		mod(r)
	}
	if err := store.Save(context.Background(), r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
}

// testClaimDecisionOnce pins that of two decisions claiming one held run the first wins and the
// second is refused, that the claim reads back as pending_approval naming the winner, that a whole
// row save from a copy taken before the claim neither erases it nor names another decision, and
// that only a run waiting unleased and uncanceled can be claimed.
func testClaimDecisionOnce(t *testing.T, newStore func() run.Store) {
	tests := []struct {
		// Mod shapes the run before the claim, nil for a plain held run.
		Mod func(*run.Run)
		// Want is the error ClaimDecision returns.
		Want error
		// WantClaimed is whether the claim wins.
		WantClaimed bool
		// ID and Claim are what the claim carries.
		ID, Claim string
	}{{ // Test 0: A plain held run is claimed.
		ID: "dec_one", Claim: `{"id":"dec_one"}`, WantClaimed: true,
	}, { // Test 1: A run that is not held cannot be claimed.
		Mod: func(r *run.Run) { r.Status = run.StatusPending },
		ID:  "dec_one", Claim: `{"id":"dec_one"}`,
	}, { // Test 2: A run whose cancel was requested cannot be claimed.
		Mod: func(r *run.Run) { r.CancelRequested = true },
		ID:  "dec_one", Claim: `{"id":"dec_one"}`,
	}, { // Test 3: A leased run cannot be claimed.
		Mod: func(r *run.Run) { r.ClaimedBy = "worker" },
		ID:  "dec_one", Claim: `{"id":"dec_one"}`,
	}, { // Test 4: A claim names its decision.
		ID: "", Claim: `{}`, Want: run.ErrNoDecision,
	}, { // Test 5: A claim says how it settles.
		ID: "dec_one", Claim: "", Want: run.ErrNoDecision,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			ctx := context.Background()
			store := newStore()
			claimHeld(t, store, "run_claim", test.Mod)
			before, err := store.Get(ctx, "run_claim")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			claimed, err := store.ClaimDecision(ctx, "run_claim", test.ID, test.Claim)
			if !errors.Is(err, test.Want) {
				t.Fatalf("ClaimDecision() error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantClaimed, claimed); diff != "" {
				t.Fatalf("ClaimDecision() claimed (-want +got):\n%s", diff)
			}
			if !test.WantClaimed {
				return
			}
			if again, err := store.ClaimDecision(ctx, "run_claim", "dec_two",
				`{"id":"dec_two"}`); err != nil || again {
				t.Errorf("a second decision's ClaimDecision() = (%v, %v), want (false, nil)", again,
					err)
			}
			// A whole-row save from a copy taken before the claim.
			if err := store.Save(ctx, before); err != nil {
				t.Fatalf("Save() of the earlier copy error = %v", err)
			}
			got, err := store.Get(ctx, "run_claim")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.Status != run.StatusPendingApproval || got.DecisionID != "dec_one" ||
				got.DecisionClaim != test.Claim || !got.InFlight() {
				t.Errorf("after the claim and a stale save the run is %s, decided by %q with claim "+
					"%q, want pending_approval in flight for dec_one", got.Status, got.DecisionID,
					got.DecisionClaim)
			}
		})
	}
}

// testClaimedRunIsFenced pins that a claimed run refuses every write that moves a held run out of
// pending_approval except its own decision's settle: a cancel, a held settle, a transition, and a
// transition that takes a lease.
func testClaimedRunIsFenced(t *testing.T, newStore func() run.Store) {
	moves := []struct {
		// Name labels the write.
		Name string
		// Move attempts it and reports whether it changed the run.
		Move func(context.Context, run.Store) (bool, error)
	}{{ // Test 0: A cancel of a waiting run.
		Name: "cancel pending",
		Move: func(ctx context.Context, s run.Store) (bool, error) {
			return s.CancelPending(ctx, "run_fenced")
		},
	}, { // Test 1: A held settle, the way an approval step was decided.
		Name: "settle held",
		Move: func(ctx context.Context, s run.Store) (bool, error) {
			return s.SettleHeld(ctx, "run_fenced", run.Finalization{Status: run.StatusRejected,
				EndedAt: time.Now()})
		},
	}, { // Test 2: A transition out of pending_approval.
		Name: "transition",
		Move: func(ctx context.Context, s run.Store) (bool, error) {
			return s.TransitionStatus(ctx, "run_fenced", run.StatusPendingApproval,
				run.StatusPending)
		},
	}, { // Test 3: A transition that takes a lease.
		Name: "transition and claim",
		Move: func(ctx context.Context, s run.Store) (bool, error) {
			return s.TransitionStatusAndClaim(ctx, "run_fenced", run.StatusPendingApproval,
				run.StatusRunning, "coordinator", time.Now())
		},
	}}
	for testNum, test := range moves {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			ctx := context.Background()
			store := newStore()
			claimHeld(t, store, "run_fenced", nil)
			if ok, err := store.ClaimDecision(ctx, "run_fenced", "dec_fence",
				`{"id":"dec_fence"}`); err != nil || !ok {
				t.Fatalf("ClaimDecision() = (%v, %v), want (true, nil)", ok, err)
			}
			moved, err := test.Move(ctx, store)
			if err != nil || moved {
				t.Errorf("%s on a claimed run = (%v, %v), want (false, nil): only the claiming "+
					"decision may move it", test.Name, moved, err)
			}
			got, err := store.Get(ctx, "run_fenced")
			if err != nil || !got.InFlight() {
				t.Errorf("after %s the run is %+v, %v, want it still claimed", test.Name, got, err)
			}
		})
	}
}

// testSettleDecision pins that only the claiming decision settles its run, once, and that each
// settlement writes what it says: a release to pending, a release to running under a lease with a
// start time, a terminal end with its failure text and time, and a terminal status alone for a
// rejection that finalize completes.
func testSettleDecision(t *testing.T, newStore func() run.Store) {
	ended := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	tests := []struct {
		// Settle is the settlement.
		Settle run.DecisionSettle
		// Want is the error SettleDecision returns.
		Want error
		// WantStatus is the run's status afterward.
		WantStatus run.Status
		// WantOwner is who holds its lease afterward.
		WantOwner string
		// WantError is its failure text afterward.
		WantError string
		// WantLease, WantStarted, and WantEnded say which times it carries afterward.
		WantLease, WantStarted, WantEnded bool
	}{{ // Test 0: An approved run released to the claim loop.
		Settle:     run.DecisionSettle{Status: run.StatusPending},
		WantStatus: run.StatusPending,
	}, { // Test 1: An approved parent released to its coordinator.
		Settle:     run.DecisionSettle{Status: run.StatusRunning, Owner: "coordinator"},
		WantStatus: run.StatusRunning, WantOwner: "coordinator", WantLease: true,
		WantStarted: true,
	}, { // Test 2: A denied step, ended whole.
		Settle: run.DecisionSettle{Status: run.StatusRejected, Error: "denied by an approver",
			EndedAt: ended},
		WantStatus: run.StatusRejected, WantError: "denied by an approver", WantEnded: true,
	}, { // Test 3: A rejected run, its end left to finalize.
		Settle:     run.DecisionSettle{Status: run.StatusRejected},
		WantStatus: run.StatusRejected,
	}, { // Test 4: Running with nobody to hold the lease is refused.
		Settle: run.DecisionSettle{Status: run.StatusRunning}, Want: run.ErrBadSettle,
		WantStatus: run.StatusPendingApproval,
	}, { // Test 5: A status no decision reaches is refused.
		Settle: run.DecisionSettle{Status: run.StatusPendingApproval}, Want: run.ErrBadSettle,
		WantStatus: run.StatusPendingApproval,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			ctx := context.Background()
			store := newStore()
			claimHeld(t, store, "run_settle", nil)
			if ok, err := store.ClaimDecision(ctx, "run_settle", "dec_settle",
				`{"id":"dec_settle"}`); err != nil || !ok {
				t.Fatalf("ClaimDecision() = (%v, %v), want (true, nil)", ok, err)
			}
			if wrong, err := store.SettleDecision(ctx, "run_settle", "dec_other",
				run.DecisionSettle{Status: run.StatusPending}); err != nil || wrong {
				t.Fatalf("another decision's SettleDecision() = (%v, %v), want (false, nil)",
					wrong, err)
			}
			settled, err := store.SettleDecision(ctx, "run_settle", "dec_settle", test.Settle)
			if !errors.Is(err, test.Want) {
				t.Fatalf("SettleDecision() error = %v, want %v", err, test.Want)
			}
			if settled != (test.Want == nil) {
				t.Fatalf("SettleDecision() settled = %v", settled)
			}
			got, err := store.Get(ctx, "run_settle")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if diff := cmp.Diff(test.WantStatus, got.Status); diff != "" {
				t.Errorf("status after the settle (-want +got):\n%s", diff)
			}
			if test.Want != nil {
				return
			}
			if got.DecisionID != "dec_settle" || got.InFlight() || got.DecisionClaim != "" {
				t.Errorf("after the settle the run is decided by %q, claim %q, want dec_settle "+
					"settled", got.DecisionID, got.DecisionClaim)
			}
			if got.ClaimedBy != test.WantOwner || got.Error != test.WantError ||
				(got.ClaimedAt != nil) != test.WantLease ||
				(got.StartedAt != nil) != test.WantStarted || (got.EndedAt != nil) != test.WantEnded {
				t.Errorf("after the settle the run is held by %q since %v, started %v, ended %v, "+
					"error %q", got.ClaimedBy, got.ClaimedAt, got.StartedAt, got.EndedAt, got.Error)
			}
			if again, err := store.SettleDecision(ctx, "run_settle", "dec_settle",
				test.Settle); err != nil || again {
				t.Errorf("a second SettleDecision() = (%v, %v), want (false, nil)", again, err)
			}
		})
	}
}

// testParkedIsNoHeldRun pins that a workflow parked at an approval step reads as held, counts and
// lists as held, and can be resumed and canceled, but is never a run a decision can claim, settle,
// or transition as one held before it started. The database stores keep it under a value of its
// own, which is how an earlier release leaves it alone.
func testParkedIsNoHeldRun(t *testing.T, store run.Store) {
	ctx := context.Background()
	for _, id := range []string{"run_parked", "run_cancel"} {
		parent := approvalParent(id, run.StatusRunning)
		parent.ClaimedBy = "coordinator-a"
		now := time.Now()
		parent.ClaimedAt = &now
		if err := store.Save(ctx, parent); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		if ok, err := store.ParkForApproval(ctx, id, "coordinator-a"); err != nil || !ok {
			t.Fatalf("ParkForApproval(%s) = (%v, %v), want (true, nil)", id, ok, err)
		}
	}
	got, err := store.Get(ctx, "run_parked")
	if err != nil || got.Status != run.StatusPendingApproval {
		t.Fatalf("a parked workflow reads as %+v, %v, want pending_approval", got, err)
	}
	if ok, err := store.ClaimDecision(ctx, "run_parked", "dec_parked",
		`{"id":"dec_parked"}`); err != nil || ok {
		t.Errorf("ClaimDecision() on a parked workflow = (%v, %v), want (false, nil): approving "+
			"it as a whole run would start it again from its first step", ok, err)
	}
	if ok, err := store.TransitionStatus(ctx, "run_parked", run.StatusPendingApproval,
		run.StatusPending); err != nil || ok {
		t.Errorf("TransitionStatus() on a parked workflow = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := store.SettleHeld(ctx, "run_parked", run.Finalization{
		Status: run.StatusRejected, EndedAt: time.Now()}); err != nil || ok {
		t.Errorf("SettleHeld() on a parked workflow = (%v, %v), want (false, nil)", ok, err)
	}
	page, err := store.ListPage(ctx, run.ListFilter{Status: string(run.StatusPendingApproval)},
		10, 0)
	if err != nil || len(page) != 2 {
		t.Errorf("ListPage(pending_approval) = %d runs, %v, want both parked workflows", len(page),
			err)
	}
	counts, err := store.RunStatusCounts(ctx)
	if err != nil || counts[run.StatusPendingApproval] != 2 {
		t.Errorf("RunStatusCounts() = %v, %v, want both parked workflows held", counts, err)
	}
	if ok, err := store.CancelPending(ctx, "run_cancel"); err != nil || !ok {
		t.Errorf("CancelPending() on a parked workflow = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := store.TransitionStatusAndClaim(ctx, "run_parked", run.StatusPendingApproval,
		run.StatusRunning, "coordinator-b", time.Time{}); err != nil || !ok {
		t.Errorf("resuming a parked workflow = (%v, %v), want (true, nil)", ok, err)
	}
	if got, err := store.Get(ctx, "run_parked"); err != nil || got.Status != run.StatusRunning ||
		got.ClaimedBy != "coordinator-b" {
		t.Errorf("after the resume the workflow is %+v, %v, want running under coordinator-b",
			got, err)
	}
}
