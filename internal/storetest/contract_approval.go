package storetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	t.Run("the sweep parks a workflow stalled at its approval step", func(t *testing.T) {
		testSweepParksAStalledWorkflow(t, newStore)
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

// testSweepParksAStalledWorkflow pins what the lease sweep does with a workflow whose coordinator
// died after opening its approval step and before parking. Its step is listed for a decision, so the
// sweep parks it the way the coordinator would have, stored as parked, with a decision already made
// or claimed left in place for the resume to act on. A workflow with other work open, or a cancel
// requested, is interrupted as before, and one whose lease is fresh is left alone.
func testSweepParksAStalledWorkflow(t *testing.T, newStore func() run.Store) {
	tests := []struct {
		// Gate is the status the approval step's record holds.
		Gate run.Status
		// Claim has a decision claim the approval step before the sweep.
		Claim bool
		// Deploy is the status of the approve path's record, empty for none.
		Deploy run.Status
		// Beside is the status of a step running beside the approval step, empty for none.
		Beside run.Status
		// Cancel requests a cancel on the workflow.
		Cancel bool
		// Fresh keeps the workflow's lease inside its lifetime.
		Fresh bool
		// WantStatus is the workflow's status after the sweep.
		WantStatus run.Status
		// WantParked reports whether the sweep parked the workflow.
		WantParked bool
		// WantGate is the approval step's status after the sweep.
		WantGate run.Status
	}{{ // Test 0: A step still waiting parks the workflow and stays waiting.
		Gate: run.StatusPendingApproval, WantStatus: run.StatusPendingApproval, WantParked: true,
		WantGate: run.StatusPendingApproval,
	}, { // Test 1: A step decided while the lease aged parks the workflow for the resume.
		Gate: run.StatusSucceeded, WantStatus: run.StatusPendingApproval, WantParked: true,
		WantGate: run.StatusSucceeded,
	}, { // Test 2: A step a decision claimed parks the workflow and stays with that decision.
		Gate: run.StatusPendingApproval, Claim: true, WantStatus: run.StatusPendingApproval,
		WantParked: true, WantGate: run.StatusPendingApproval,
	}, { // Test 3: A step still executing beside the waiting step is interrupted, and so is the step.
		Gate: run.StatusPendingApproval, Beside: run.StatusRunning,
		WantStatus: run.StatusInterrupted, WantGate: run.StatusCanceled,
	}, { // Test 4: A workflow somebody asked to cancel is interrupted and its step withdrawn.
		Gate: run.StatusPendingApproval, Cancel: true, WantStatus: run.StatusInterrupted,
		WantGate: run.StatusCanceled,
	}, { // Test 5: A workflow whose lease is fresh is left to its coordinator.
		Gate: run.StatusPendingApproval, Fresh: true, WantStatus: run.StatusRunning,
		WantGate: run.StatusPendingApproval,
	}, { // Test 6: A workflow whose approval was acted on and finished was waiting for nobody.
		Gate: run.StatusSucceeded, Deploy: run.StatusSucceeded, WantStatus: run.StatusInterrupted,
		WantGate: run.StatusSucceeded,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			ctx := context.Background()
			store := newStore()
			reporter, ok := store.(settledReporter)
			if !ok {
				t.Fatalf("%T does not implement ReclaimStaleSettled", store)
			}
			leased := time.Now().Add(-2 * time.Hour)
			if test.Fresh {
				leased = time.Now()
			}
			parent := approvalParent("run_stall", run.StatusRunning)
			parent.ClaimedBy, parent.ClaimedAt = "gone", &leased
			if test.Beside != "" {
				parent.Steps = append(parent.Steps, run.PipelineStep{Name: "lint", Tool: "bash",
					Command: "lint"})
			}
			started := parent.CreatedAt.Add(time.Second)
			parent.StartedAt = &started
			build, deploy, lint := 0, 2, 4
			records := []*run.Run{parent, {ID: "run_stall_build", Playbook: "release",
				ParentID: &parent.ID, StepIndex: &build, StepName: "build",
				Status: run.StatusSucceeded, CreatedAt: started}}
			gate := approvalStep("run_stall_gate", parent.ID)
			gate.Status = test.Gate
			records = append(records, gate)
			if test.Deploy != "" {
				fresh := time.Now()
				records = append(records, &run.Run{ID: "run_stall_deploy", Playbook: "deploy.yml",
					ParentID: &parent.ID, StepIndex: &deploy, StepName: "deploy",
					Status: test.Deploy, ClaimedBy: "relay", ClaimedAt: &fresh,
					CreatedAt: gate.CreatedAt.Add(time.Second)})
			}
			if test.Beside != "" {
				fresh := time.Now()
				records = append(records, &run.Run{ID: "run_stall_lint", Playbook: "release",
					ParentID: &parent.ID, StepIndex: &lint, StepName: "lint",
					Status: test.Beside, ClaimedBy: "relay", ClaimedAt: &fresh, CreatedAt: started})
			}
			for _, r := range records {
				if err := store.Save(ctx, r); err != nil {
					t.Fatalf("Save(%s) error = %v", r.ID, err)
				}
			}
			if test.Claim {
				if ok, err := store.ClaimDecision(ctx, gate.ID, "dec_stall",
					`{"id":"dec_stall"}`); err != nil || !ok {
					t.Fatalf("ClaimDecision() = (%v, %v), want (true, nil)", ok, err)
				}
			}
			if test.Cancel {
				if err := store.RequestCancel(ctx, parent.ID); err != nil {
					t.Fatalf("RequestCancel() error = %v", err)
				}
			}
			_, settled, err := reporter.ReclaimStaleSettled(ctx, time.Minute)
			if err != nil {
				t.Fatalf("ReclaimStaleSettled() error = %v", err)
			}
			got, err := store.Get(ctx, parent.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.Status != test.WantStatus {
				t.Errorf("workflow after the sweep = %q (%s), want %q", got.Status, got.Error,
					test.WantStatus)
			}
			wantSettled := test.WantStatus.Terminal()
			if named := slices.Contains(settled, parent.ID); named != wantSettled {
				t.Errorf("settled = %v, want the workflow named %v", settled, wantSettled)
			}
			if test.WantParked {
				if got.ClaimedBy != "" || got.ClaimedAt != nil {
					t.Errorf("a parked workflow kept its lease: %q at %v", got.ClaimedBy,
						got.ClaimedAt)
				}
				// Stored as parked rather than as a plain hold, so no decision can take it as a
				// whole run held before it started.
				if ok, err := store.ClaimDecision(ctx, parent.ID, "dec_whole",
					`{"id":"dec_whole"}`); err != nil || ok {
					t.Errorf("ClaimDecision() on the parked workflow = (%v, %v), want (false, nil)",
						ok, err)
				}
			}
			step, err := store.Get(ctx, gate.ID)
			if err != nil {
				t.Fatalf("Get(%s) error = %v", gate.ID, err)
			}
			if step.Status != test.WantGate || step.InFlight() != test.Claim {
				t.Errorf("approval step after the sweep = %q in flight %v, want %q in flight %v",
					step.Status, step.InFlight(), test.WantGate, test.Claim)
			}
		})
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
