package run

import (
	"fmt"
	"testing"
	"time"
)

// stallGraph is build, then an approval step with an approve path and a deny path, beside a lint
// step and a report that waits for it.
func stallGraph() []PipelineStep {
	return []PipelineStep{
		{Name: "build", Tool: "bash", Command: "make"},
		{Name: "gate", Type: StepApproval, DependsOn: []string{"build"}},
		{Name: "deploy", Tool: "bash", Command: "deploy", DependsOn: []string{"gate"}},
		{Name: "notify", Tool: "bash", Command: "notify", IfDenied: []string{"gate"}},
		{Name: "lint", Tool: "bash", Command: "lint"},
		{Name: "report", Tool: "bash", Command: "report", DependsOn: []string{"lint"}},
	}
}

// TestStalledAtApproval pins which workflows the lease sweep parks: one whose records show nothing
// left to do but wait for a person at an approval step it asked, whether the step is still waiting
// or was decided since and not acted on. Anything executing or ready to start, a cancel, or a step
// never asked keeps the interrupt.
func TestStalledAtApproval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Steps replaces stallGraph when set.
		Steps []PipelineStep
		// Records maps a step's index to the status of its record. A missing index has no record.
		Records map[int]Status
		// Claimed marks the approval step's record as claimed by a decision.
		Claimed bool
		// Cancel requests a cancel on the workflow.
		Cancel bool
		// Kind replaces the workflow's kind when set.
		Kind string
		// WantStalled is whether the sweep parks the workflow.
		WantStalled bool
	}{{ // Test 0: The step waits and everything else finished.
		Records: map[int]Status{0: StatusSucceeded, 1: StatusPendingApproval,
			4: StatusSucceeded, 5: StatusSucceeded},
		WantStalled: true,
	}, { // Test 1: The step was approved and the approve path has no record yet.
		Records: map[int]Status{0: StatusSucceeded, 1: StatusSucceeded,
			4: StatusSucceeded, 5: StatusSucceeded},
		WantStalled: true,
	}, { // Test 2: The step was denied and the deny path has no record yet.
		Records: map[int]Status{0: StatusSucceeded, 1: StatusRejected,
			4: StatusSucceeded, 5: StatusSucceeded},
		WantStalled: true,
	}, { // Test 3: The approval was acted on and every step finished, so nobody is waited for.
		Records: map[int]Status{0: StatusSucceeded, 1: StatusSucceeded, 2: StatusSucceeded,
			4: StatusSucceeded, 5: StatusSucceeded},
		WantStalled: false,
	}, { // Test 4: The approve path is executing.
		Records: map[int]Status{0: StatusSucceeded, 1: StatusSucceeded, 2: StatusRunning,
			4: StatusSucceeded, 5: StatusSucceeded},
		WantStalled: false,
	}, { // Test 5: Another branch is executing beside the waiting step.
		Records: map[int]Status{0: StatusSucceeded, 1: StatusPendingApproval,
			4: StatusSucceeded, 5: StatusRunning},
		WantStalled: false,
	}, { // Test 6: Another branch has a step ready to start.
		Records:     map[int]Status{0: StatusSucceeded, 1: StatusPendingApproval, 4: StatusSucceeded},
		WantStalled: false,
	}, { // Test 7: A step its failed dependency rules out is not work left to do.
		Records:     map[int]Status{0: StatusSucceeded, 1: StatusPendingApproval, 4: StatusFailed},
		WantStalled: true,
	}, { // Test 8: The step was never asked, so nobody could have decided it.
		Records:     map[int]Status{0: StatusSucceeded, 4: StatusSucceeded, 5: StatusSucceeded},
		WantStalled: false,
	}, { // Test 9: A decision has claimed the step and not settled it.
		Records: map[int]Status{0: StatusSucceeded, 1: StatusPendingApproval,
			4: StatusSucceeded, 5: StatusSucceeded},
		Claimed: true, WantStalled: true,
	}, { // Test 10: Somebody asked to cancel the workflow.
		Records: map[int]Status{0: StatusSucceeded, 1: StatusPendingApproval,
			4: StatusSucceeded, 5: StatusSucceeded},
		Cancel: true, WantStalled: false,
	}, { // Test 11: A split is never a workflow at an approval step.
		Records: map[int]Status{0: StatusSucceeded, 1: StatusPendingApproval,
			4: StatusSucceeded, 5: StatusSucceeded},
		Kind: KindSplit, WantStalled: false,
	}, { // Test 12: A second approval step ready to be asked waits for a person as well.
		Steps: []PipelineStep{
			{Name: "build", Tool: "bash", Command: "make"},
			{Name: "gate", Type: StepApproval, DependsOn: []string{"build"}},
			{Name: "signoff", Type: StepApproval, DependsOn: []string{"build"}},
		},
		Records:     map[int]Status{0: StatusSucceeded, 1: StatusPendingApproval},
		WantStalled: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			steps := test.Steps
			if steps == nil {
				steps = stallGraph()
			}
			parent := &Run{ID: "run_wf", Kind: KindPipeline, Status: StatusRunning, Steps: steps,
				CancelRequested: test.Cancel}
			if test.Kind != "" {
				parent.Kind = test.Kind
			}
			var records []*Run
			for idx, status := range test.Records {
				r := &Run{ID: fmt.Sprintf("run_step_%d", idx), ParentID: &parent.ID,
					StepIndex: &idx, StepName: steps[idx].Name, Status: status,
					CreatedAt: time.Date(2026, 1, 2, 3, 4, idx, 0, time.UTC)}
				if steps[idx].IsApproval() {
					r.Kind = KindApproval
					if test.Claimed {
						r.DecisionClaim = `{"id":"dec_step"}`
					}
				}
				records = append(records, r)
			}
			if got := StalledAtApproval(parent, records); got != test.WantStalled {
				t.Errorf("StalledAtApproval() = %v, want %v", got, test.WantStalled)
			}
		})
	}
}
