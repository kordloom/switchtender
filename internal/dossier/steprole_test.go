package dossier

import (
	"fmt"
	"testing"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
)

// TestWorkflowApprovalStepEntriesAreNamed pins how a workflow approval step's chain entries read in
// a dossier and a register. The dossier names each by what happened at the step, and the register
// credits an approval or a denial to the workflow, so a workflow that waited for a person mid-run
// does not read as one nobody decided on. A request and a timeout are not anybody's decision.
func TestWorkflowApprovalStepEntriesAreNamed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Verdict is the step entry's verdict.
		Verdict string
		// WantRole is how the dossier names the entry.
		WantRole string
		// WantID is the run the register credits, empty for none.
		WantID string
		// WantVerdict is the decision the register records.
		WantVerdict string
	}{{ // Test 0: The request is named, and is nobody's decision.
		Verdict: outcome.StepRequested, WantRole: "Step approval requested",
	}, { // Test 1: An approval is named and credited to the workflow.
		Verdict: outcome.StepApproved, WantRole: "Step approved", WantID: "run_wf",
		WantVerdict: "Approved",
	}, { // Test 2: A denial is named and credited to the workflow.
		Verdict: outcome.StepRejected, WantRole: "Step denied", WantID: "run_wf",
		WantVerdict: "Rejected",
	}, { // Test 3: A timeout is named, and is nobody's decision.
		Verdict: outcome.StepTimedOut, WantRole: "Step timed out",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			e := &audit.Entry{Method: audit.MethodDecision,
				Path: outcome.StepDecisionPath("run_wf", "run_gate", test.Verdict)}
			if got := entryRole(e); got != test.WantRole {
				t.Errorf("entryRole() = %q, want %q", got, test.WantRole)
			}
			id, verdict := decisionOf(e)
			if id != test.WantID || verdict != test.WantVerdict {
				t.Errorf("decisionOf() = (%q, %q), want (%q, %q)", id, verdict, test.WantID,
					test.WantVerdict)
			}
		})
	}
}
