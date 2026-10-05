package policy

import (
	"fmt"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestOnlyAnUnplannedApplyIsPlanGated holds plan gating to the runs the plan gate takes: a
// Terraform or OpenTofu apply nothing has planned yet. The dispatcher leaves a plan-gated run
// unheld and trusts the plan gate to hold what it proposes, so a plan-content rule that matched any
// other run released it from every hold. The control is the unplanned apply, which is gated.
func TestOnlyAnUnplannedApplyIsPlanGated(t *testing.T) {
	t.Parallel()
	anyTool := &Policy{ID: "pol_limit", Name: "destroy limit", MaxDestroy: 3}
	terraform := &Policy{ID: "pol_tf", Name: "tf limit", Tool: run.ToolTerraform, MaxDestroy: 3}
	tests := []struct {
		// Policies are the rules in force.
		Policies []*Policy
		// Run is the run asked about.
		Run *run.Run
		// WantGated is whether it is planned first.
		WantGated bool
	}{{ // Test 0: An Ansible run is never planned, whatever a destroy limit with no tool says.
		Policies: []*Policy{anyTool}, Run: &run.Run{Tool: run.ToolAnsible, Playbook: "site.yml"},
	}, { // Test 1: Nor is a bash run.
		Policies: []*Policy{anyTool}, Run: &run.Run{Tool: run.ToolBash, Command: "./deploy.sh"},
	}, { // Test 2: An apply a plan already proposed is not planned again.
		Policies: []*Policy{terraform},
		Run:      &run.Run{Tool: run.ToolTerraform, Command: "infra", ProposedFrom: "run_plan"},
	}, { // Test 3: Nor is a plan, which is the dry run.
		Policies: []*Policy{terraform},
		Run:      &run.Run{Tool: run.ToolTerraform, Command: "infra", DryRun: true},
	}, { // Test 4: An agent's proposed apply is not planned again either.
		Policies: nil,
		Run: &run.Run{Tool: run.ToolTerraform, Command: "infra", ProposedFrom: "run_plan",
			ActorType: ActorKindAgent},
	}, { // Test 5: An unplanned apply is planned first, the control.
		Policies: []*Policy{terraform}, Run: &run.Run{Tool: run.ToolTerraform, Command: "infra"},
		WantGated: true,
	}, { // Test 6: So is one under a destroy limit with no tool.
		Policies: []*Policy{anyTool}, Run: &run.Run{Tool: run.ToolOpenTofu, Command: "infra"},
		WantGated: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := PlanGated(test.Policies, test.Run); got != test.WantGated {
				t.Errorf("PlanGated() = %v, want %v", got, test.WantGated)
			}
		})
	}
}
