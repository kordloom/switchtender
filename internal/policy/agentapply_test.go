package policy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestAnAgentsApplyPlansFirst holds which runs the built-in hold keeps waiting before anything
// plans: an agent's Terraform or OpenTofu apply that nothing has planned yet and no exemption
// covers. A dry run, the apply a plan proposed, and a step of a workflow are not, and neither is a
// person's apply or an exempt agent's, which are the controls.
func TestAnAgentsApplyPlansFirst(t *testing.T) {
	t.Parallel()
	exempt := exemption("tf bot", func(p *Policy) { p.Tool = "terraform" })
	step := "run_flow"
	tests := []struct {
		// Policies are the rules in force.
		Policies []*Policy
		// Run is the run asked about.
		Run *run.Run
		// WantFirst is whether it waits before it plans.
		WantFirst bool
	}{{ // Test 0: An agent's Terraform apply.
		Run: agentRun("bot", "terraform", "infra"), WantFirst: true,
	}, { // Test 1: An agent's OpenTofu apply.
		Run: agentRun("bot", "opentofu", "infra"), WantFirst: true,
	}, { // Test 2: A person's apply, the control.
		Run: personRun("terraform", "infra"),
	}, { // Test 3: An exempt agent's apply.
		Policies: []*Policy{exempt}, Run: agentRun("bot", "terraform", "infra"),
	}, { // Test 4: An agent's plan, which is a dry run and simply waits.
		Run: &run.Run{Actor: "bot", ActorType: ActorKindAgent, Tool: "terraform",
			Command: "infra", DryRun: true},
	}, { // Test 5: The apply an agent's plan proposed.
		Run: &run.Run{Actor: "bot", ActorType: ActorKindAgent, Tool: "terraform",
			Command: "infra", ProposedFrom: "run_plan"},
	}, { // Test 6: A step of an agent's workflow, which its workflow's approval governs.
		Run: &run.Run{Actor: "bot", ActorType: ActorKindAgent, Tool: "terraform",
			Command: "infra", ParentID: &step},
	}, { // Test 7: An agent's bash run, which is not planned at all.
		Run: agentRun("bot", "bash", "./deploy.sh"),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := AgentPlansFirst(test.Policies, test.Run); got != test.WantFirst {
				t.Errorf("AgentPlansFirst() = %v, want %v", got, test.WantFirst)
			}
		})
	}
}

// TestAgentHoldReasonSaysWhyItWaits holds the reason the built-in hold gives for a run that is
// meant to change nothing yet: what the agent asked for runs code with this server's credentials,
// so it waits for a person or an exemption. A plain change gets no reason beyond the hold's name,
// which is the control.
func TestAgentHoldReasonSaysWhyItWaits(t *testing.T) {
	t.Parallel()
	dry := func(tool string) *run.Run {
		return &run.Run{Actor: "bot", ActorType: ActorKindAgent, Tool: tool, DryRun: true}
	}
	tests := []struct {
		// Run is the held run.
		Run *run.Run
		// WantParts are what the reason must say, none when it must be empty.
		WantParts []string
	}{{ // Test 0: Check mode runs code on the controller.
		Run:       dry(run.ToolAnsible),
		WantParts: []string{"check mode still runs lookups", "this server's credentials"},
	}, { // Test 1: A plan runs provider code.
		Run:       dry(run.ToolOpenTofu),
		WantParts: []string{"a plan still runs provider code", "this server's credentials"},
	}, { // Test 2: Any other dry run still runs its tool.
		Run:       dry(run.ToolBash),
		WantParts: []string{"a dry run still runs code", "exemption"},
	}, { // Test 3: An apply waits before it plans and says what the two approvals are.
		Run:       agentRun("bot", "terraform", "infra"),
		WantParts: []string{"before anything plans", "second approval", "saved plan"},
	}, { // Test 4: The proposed apply says what its approval binds.
		Run: &run.Run{Actor: "bot", ActorType: ActorKindAgent, Tool: "terraform",
			ProposedFrom: "run_plan"},
		WantParts: []string{"carrying the saved plan", "applies exactly that plan"},
	}, { // Test 5: A plain change says nothing more, the control.
		Run: agentRun("bot", "bash", "./deploy.sh"),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := AgentHoldReason(test.Run)
			if (got == "") != (len(test.WantParts) == 0) {
				t.Fatalf("AgentHoldReason() = %q, want parts %q", got, test.WantParts)
			}
			for _, want := range test.WantParts {
				if !strings.Contains(got, want) {
					t.Errorf("AgentHoldReason() = %q, want it to say %q", got, want)
				}
			}
		})
	}
}
