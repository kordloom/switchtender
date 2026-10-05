package attention

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestHeldBySaysWhatHeldTheRun holds the end of the sentence an attention alert writes about a held
// run. A stored rule is quoted by name, and the built-in hold on an agent's run is given as the
// reason it is, since "held by rule" named a rule nobody wrote and nobody can find in the list.
func TestHeldBySaysWhatHeldTheRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Rule is what the run records as having held it.
		Rule string
		// WantText is the end of the sentence.
		WantText string
	}{{ // Test 0: The built-in hold reads as its reason.
		Rule: policy.AgentDefaultName, WantText: ": requested by an agent, held by default",
	}, { // Test 1: A stored rule is quoted by name, the control.
		Rule: "prod waits", WantText: ` by rule "prod waits"`,
	}, { // Test 2: Nothing named adds nothing.
		Rule: "", WantText: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := heldBy(test.Rule); got != test.WantText {
				t.Errorf("heldBy(%q) = %q, want %q", test.Rule, got, test.WantText)
			}
		})
	}
}

// TestAnAlertSaysWhyAnAgentsPreviewWaits holds an alert about a run the built-in hold keeps waiting
// to saying why when what the agent asked for runs code with this server's credentials: a dry run
// or an apply nothing has planned. A plain change and a run a stored rule held add nothing, which
// are the controls.
func TestAnAlertSaysWhyAnAgentsPreviewWaits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Run is the held run.
		Run *run.Run
		// WantPart is what the alert adds, empty when it adds nothing.
		WantPart string
	}{{ // Test 0: An agent's check-mode run.
		Run: &run.Run{Tool: run.ToolAnsible, DryRun: true, ActorType: policy.ActorKindAgent,
			HeldByPolicy: policy.AgentDefaultName},
		WantPart: "check mode still runs lookups",
	}, { // Test 1: An agent's apply nothing has planned.
		Run: &run.Run{Tool: run.ToolTerraform, ActorType: policy.ActorKindAgent,
			HeldByPolicy: policy.AgentDefaultName},
		WantPart: "before anything plans",
	}, { // Test 2: An agent's plain change, the control.
		Run: &run.Run{Tool: run.ToolBash, ActorType: policy.ActorKindAgent,
			HeldByPolicy: policy.AgentDefaultName},
	}, { // Test 3: A dry run a stored rule held, the control.
		Run: &run.Run{Tool: run.ToolAnsible, DryRun: true, HeldByPolicy: "hold ansible"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := agentHoldWhy(test.Run)
			if (got == "") != (test.WantPart == "") || !strings.Contains(got, test.WantPart) {
				t.Errorf("agentHoldWhy() = %q, want it to say %q", got, test.WantPart)
			}
		})
	}
}
