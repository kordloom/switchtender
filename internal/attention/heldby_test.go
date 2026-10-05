package attention

import (
	"fmt"
	"testing"

	"github.com/kordloom/switchtender/internal/policy"
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
