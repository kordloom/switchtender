package dispatch

import (
	"context"
	"fmt"
	"testing"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestADestroyLimitReleasesNoOtherRun holds a plan-content rule to the applies it weighs. A destroy
// limit matched runs the plan gate never takes, and a run it matched was read as one the plan gate
// would hold later, so the gate in front of it let it through: a bash run waited for nobody under a
// rule holding everything, so did the apply a plan proposed beside a destroy limit written for
// Terraform, and an agent's run waited for nobody under the default hold. The controls are the same
// runs with no destroy limit in force, which are held.
func TestADestroyLimitReleasesNoOtherRun(t *testing.T) {
	t.Parallel()
	limit := &policy.Policy{ID: "pol_limit", Name: "destroy limit", MaxDestroy: 3}
	terraformLimit := &policy.Policy{ID: "pol_tf_limit", Name: "terraform destroy limit",
		Tool: run.ToolTerraform, MaxDestroy: 3}
	holdAll := &policy.Policy{ID: "pol_all", Name: "hold everything",
		MaxDestroy: policy.DisabledMaxDestroy}
	bash := []run.SubmitOption{run.WithTool(run.ToolBash), run.WithCommand("./deploy.sh")}
	proposed := []run.SubmitOption{run.WithTool(run.ToolTerraform), run.WithCommand("infra"),
		run.WithProposedFrom("run_plan")}
	tests := []struct {
		// Rules are the stored policies.
		Rules []*policy.Policy
		// Opts say who submits and what.
		Opts []run.SubmitOption
		// WantHeldBy names what holds the run.
		WantHeldBy string
	}{{ // Test 0: A person's bash run under a rule holding everything stays held.
		Rules: []*policy.Policy{holdAll, limit}, Opts: append(personOpts(), bash...),
		WantHeldBy: "hold everything",
	}, { // Test 1: The apply a plan proposed stays held beside a Terraform destroy limit.
		Rules: []*policy.Policy{holdAll, terraformLimit}, Opts: append(personOpts(), proposed...),
		WantHeldBy: "hold everything",
	}, { // Test 2: An agent's run stays held by default.
		Rules: []*policy.Policy{limit}, Opts: append(agentOpts(), bash...),
		WantHeldBy: policy.AgentDefaultName,
	}, { // Test 3: With no destroy limit in force the person's run is held, the control.
		Rules: []*policy.Policy{holdAll}, Opts: append(personOpts(), bash...),
		WantHeldBy: "hold everything",
	}, { // Test 4: So is the proposed apply, the control.
		Rules: []*policy.Policy{holdAll}, Opts: append(personOpts(), proposed...),
		WantHeldBy: "hold everything",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			d := newAgentHoldDispatcher(t, run.NewMemStore(), rulesHolding(t, test.Rules...))
			got, err := d.Submit(context.Background(), "", "", test.Opts...)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if got.Status != run.StatusPendingApproval || got.HeldByPolicy != test.WantHeldBy {
				t.Errorf("run = %s held by %q, want held by %q", got.Status, got.HeldByPolicy,
					test.WantHeldBy)
			}
		})
	}
}
