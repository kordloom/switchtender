package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// approvalContract runs the store behavior a workflow paused at an approval step depends on. It is
// called from Contract, so every backend answers the same questions.
func approvalContract(t *testing.T, newStore func() run.Store) {
	t.Helper()
	t.Run("approval steps round trip", func(t *testing.T) {
		testApprovalStepsRoundTrip(t, newStore())
	})
	t.Run("park for approval is fenced", func(t *testing.T) { testParkForApproval(t, newStore) })
	t.Run("settle held is one compare and swap", func(t *testing.T) { testSettleHeld(t, newStore) })
	t.Run("a parked workflow survives the sweep", func(t *testing.T) {
		testParkedWorkflowSurvivesTheSweep(t, newStore())
	})
	t.Run("an approval step is never claimed", func(t *testing.T) {
		testApprovalStepIsNeverClaimed(t, newStore())
	})
	decisionClaimContract(t, newStore)
}

// approvalParent returns a pipeline parent carrying an approval step and a deny path, with times
// fixed far enough in the past that every sweep cutoff is behind them.
func approvalParent(id string, status run.Status) *run.Run {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return &run.Run{
		ID: id, Playbook: "release", Kind: run.KindPipeline, Status: status, CreatedAt: created,
		Steps: []run.PipelineStep{
			{Name: "build", Tool: "bash", Command: "make"},
			{Name: "gate", Type: run.StepApproval, Description: "Ship to production?",
				ApprovalTimeout: 3600, DependsOn: []string{"build"}},
			{Name: "deploy", Playbook: "deploy.yml", DependsOn: []string{"gate"}},
			{Name: "notify", Tool: "bash", Command: "echo denied", IfDenied: []string{"gate"}},
		},
	}
}

// approvalStep returns the record of the approval step of approvalParent.
func approvalStep(id, parentID string) *run.Run {
	idx := 1
	return &run.Run{
		ID: id, Kind: run.KindApproval, Status: run.StatusPendingApproval, StepName: "gate",
		StepIndex: &idx, ParentID: &parentID, Timeout: 3600,
		CreatedAt: time.Date(2026, 1, 2, 3, 5, 0, 0, time.UTC),
	}
}

// testApprovalStepsRoundTrip pins that a stored graph keeps every approval field. The graph is what
// a parked workflow resumes from, so a backend that dropped the deny path or the timeout would
// resume a different workflow from the one that was approved.
func testApprovalStepsRoundTrip(t *testing.T, store run.Store) {
	ctx := context.Background()
	parent := approvalParent("run_wf_rt", run.StatusRunning)
	if err := store.Save(ctx, parent); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	node := approvalStep("run_wf_rt_gate", parent.ID)
	if err := store.Save(ctx, node); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Get(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(parent.Steps, got.Steps, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("steps mismatch (-want +got):\n%s", diff)
	}
	steps, err := run.ApprovalSteps(ctx, store, parent.ID)
	if err != nil {
		t.Fatalf("ApprovalSteps() error = %v", err)
	}
	if len(steps) != 1 || steps[0].ID != node.ID || steps[0].Kind != run.KindApproval ||
		steps[0].Timeout != 3600 || steps[0].StepName != "gate" {
		t.Errorf("approval steps = %+v, want the one gate record with its timeout", steps)
	}
}

// testParkForApproval pins the write a workflow parks with: only the owner of a running parent with
// no cancel request may park it, and parking clears the lease in the same write.
func testParkForApproval(t *testing.T, newStore func() run.Store) {
	tests := []struct {
		// Status is the parent's stored status.
		Status run.Status
		// Owner holds the lease.
		Owner string
		// Caller is the process asking to park.
		Caller string
		// Cancel requests a cancel before the park.
		Cancel bool
		// WantMoved reports whether the write changed the row.
		WantMoved bool
		// WantStatus is the stored status afterward.
		WantStatus run.Status
		// WantOwner is the stored lease holder afterward.
		WantOwner string
	}{{ // Test 0: The owner of a running parent parks it and its lease is cleared.
		Status: run.StatusRunning, Owner: "node-a", Caller: "node-a",
		WantMoved: true, WantStatus: run.StatusPendingApproval, WantOwner: "",
	}, { // Test 1: Another process cannot park a parent it does not hold.
		Status: run.StatusRunning, Owner: "node-a", Caller: "node-b",
		WantMoved: false, WantStatus: run.StatusRunning, WantOwner: "node-a",
	}, { // Test 2: A parent somebody asked to cancel is not parked out of the cancel's reach.
		Status: run.StatusRunning, Owner: "node-a", Caller: "node-a", Cancel: true,
		WantMoved: false, WantStatus: run.StatusRunning, WantOwner: "node-a",
	}, { // Test 3: A parent that already ended is not parked.
		Status: run.StatusSucceeded, Owner: "node-a", Caller: "node-a",
		WantMoved: false, WantStatus: run.StatusSucceeded, WantOwner: "node-a",
	}, { // Test 4: An empty owner never matches an unleased parent.
		Status: run.StatusRunning, Owner: "", Caller: "",
		WantMoved: false, WantStatus: run.StatusRunning, WantOwner: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			ctx := context.Background()
			store := newStore()
			parent := approvalParent("run_park", test.Status)
			parent.ClaimedBy = test.Owner
			if test.Owner != "" {
				at := time.Now()
				parent.ClaimedAt = &at
			}
			if err := store.Save(ctx, parent); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			if test.Cancel {
				if err := store.RequestCancel(ctx, parent.ID); err != nil {
					t.Fatalf("RequestCancel() error = %v", err)
				}
			}
			moved, err := store.ParkForApproval(ctx, parent.ID, test.Caller)
			if err != nil {
				t.Fatalf("ParkForApproval() error = %v", err)
			}
			if moved != test.WantMoved {
				t.Errorf("ParkForApproval() moved = %v, want %v", moved, test.WantMoved)
			}
			got, err := store.Get(ctx, parent.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.Status != test.WantStatus || got.ClaimedBy != test.WantOwner {
				t.Errorf("after park = (%q, %q), want (%q, %q)", got.Status, got.ClaimedBy,
					test.WantStatus, test.WantOwner)
			}
			if test.WantMoved && got.ClaimedAt != nil {
				t.Errorf("a parked parent kept its lease time %v", got.ClaimedAt)
			}
		})
	}
	moved, err := newStore().ParkForApproval(context.Background(), "run_missing", "x")
	if err != nil || moved {
		t.Errorf("parking a missing run = (%v, %v), want (false, nil)", moved, err)
	}
}

// testSettleHeld pins the write an approval step is decided with. It is a compare-and-swap from
// pending_approval, so of two deciders exactly one wins, and the end time lands with the status.
func testSettleHeld(t *testing.T, newStore func() run.Store) {
	ended := time.Date(2026, 1, 2, 4, 0, 0, 0, time.UTC)
	tests := []struct {
		// Start is the step's stored status.
		Start run.Status
		// Claimed is a lease holder on the step, empty for none.
		Claimed string
		// Fin is the settle written.
		Fin run.Finalization
		// WantMoved reports whether the write changed the row.
		WantMoved bool
		// WantStatus is the stored status afterward.
		WantStatus run.Status
		// WantError is the stored failure text afterward.
		WantError string
		// Want is the error expected.
		Want error
	}{{ // Test 0: An approval settles a waiting step as succeeded with its end time.
		Start:     run.StatusPendingApproval,
		Fin:       run.Finalization{Status: run.StatusSucceeded, EndedAt: ended},
		WantMoved: true, WantStatus: run.StatusSucceeded,
	}, { // Test 1: A denial records its reason in the same write.
		Start:     run.StatusPendingApproval,
		Fin:       run.Finalization{Status: run.StatusRejected, Error: "not today", EndedAt: ended},
		WantMoved: true, WantStatus: run.StatusRejected, WantError: "not today",
	}, { // Test 2: A step another decider already settled is not settled again.
		Start: run.StatusSucceeded, Fin: run.Finalization{Status: run.StatusFailed, EndedAt: ended},
		WantMoved: false, WantStatus: run.StatusSucceeded,
	}, { // Test 3: A leased run is not a waiting one.
		Start: run.StatusPendingApproval, Claimed: "node-a",
		Fin:       run.Finalization{Status: run.StatusSucceeded, EndedAt: ended},
		WantMoved: false, WantStatus: run.StatusPendingApproval,
	}, { // Test 4: A status that does not end the run is refused as a caller's mistake.
		Start:     run.StatusPendingApproval,
		Fin:       run.Finalization{Status: run.StatusRunning, EndedAt: ended},
		WantMoved: false, WantStatus: run.StatusPendingApproval, Want: run.ErrNotTerminal,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			ctx := context.Background()
			store := newStore()
			if err := store.Save(ctx, approvalParent("run_settle", run.StatusRunning)); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			node := approvalStep("run_settle_gate", "run_settle")
			node.Status = test.Start
			node.ClaimedBy = test.Claimed
			if err := store.Save(ctx, node); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			moved, err := store.SettleHeld(ctx, node.ID, test.Fin)
			if !errors.Is(err, test.Want) {
				t.Fatalf("SettleHeld() error = %v, want %v", err, test.Want)
			}
			if moved != test.WantMoved {
				t.Errorf("SettleHeld() moved = %v, want %v", moved, test.WantMoved)
			}
			got, err := store.Get(ctx, node.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.Status != test.WantStatus || got.Error != test.WantError {
				t.Errorf("after settle = (%q, %q), want (%q, %q)", got.Status, got.Error,
					test.WantStatus, test.WantError)
			}
			if test.WantMoved && (got.EndedAt == nil || !got.EndedAt.Equal(ended)) {
				t.Errorf("settled step ended at %v, want %v", got.EndedAt, ended)
			}
			if test.WantMoved {
				again, err := store.SettleHeld(ctx, node.ID, run.Finalization{
					Status: run.StatusFailed, EndedAt: ended})
				if err != nil || again {
					t.Errorf("a second settle = (%v, %v), want (false, nil)", again, err)
				}
			}
		})
	}
}

// testParkedWorkflowSurvivesTheSweep pins that the lease sweep leaves a parked workflow and its
// waiting approval step alone. A parked parent holds no lease and is older than any cutoff, which
// is the shape of an abandoned parent except for its status, so a sweep that tested anything looser
// than pending and running would cancel every workflow somebody had not yet approved.
func testParkedWorkflowSurvivesTheSweep(t *testing.T, store run.Store) {
	ctx := context.Background()
	parent := approvalParent("run_parked", run.StatusPendingApproval)
	started := parent.CreatedAt.Add(time.Second)
	parent.StartedAt = &started
	if err := store.Save(ctx, parent); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	node := approvalStep("run_parked_gate", parent.ID)
	if err := store.Save(ctx, node); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := store.ReclaimStale(ctx, time.Millisecond); err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}
	for _, id := range []string{parent.ID, node.ID} {
		got, err := store.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", id, err)
		}
		if got.Status != run.StatusPendingApproval {
			t.Errorf("%s status after the sweep = %q, want pending_approval", id, got.Status)
		}
	}
}

// testApprovalStepIsNeverClaimed pins that no worker leases an approval step, even one that reads
// as pending. It executes nothing, and a worker that took one would run an empty playbook and
// record the approval as executed.
func testApprovalStepIsNeverClaimed(t *testing.T, store run.Store) {
	ctx := context.Background()
	if err := store.Save(ctx, approvalParent("run_noclaim", run.StatusRunning)); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	node := approvalStep("run_noclaim_gate", "run_noclaim")
	node.Status = run.StatusPending
	if err := store.Save(ctx, node); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if got, err := store.Claim(ctx, "node-a", nil); !errors.Is(err, run.ErrNonePending) {
		t.Errorf("Claim() = (%v, %v), want ErrNonePending", got, err)
	}
}
