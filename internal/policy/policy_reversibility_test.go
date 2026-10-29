package policy

import (
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestAReversibilityFloorIsCheckedWhereItIsWritten pins that the reversibility vocabulary is
// validated when the rule loads. How the floor is evaluated is the paid rule engine's, and is tested
// with it.
func TestAReversibilityFloorIsCheckedWhereItIsWritten(t *testing.T) {
	t.Parallel()
	gate := &Policy{Name: "two eyes on the permanent", Effect: EffectRequireApproval,
		Reversibility: run.Irreversible}
	if err := gate.Validate(); err != nil {
		t.Fatalf("policy did not validate: %v", err)
	}
	// A typo is refused when the file loads, not silently at match time. Left to match time the
	// rule loads, lists, and never fires.
	typo := &Policy{Name: "typo", Effect: EffectRequireApproval, Reversibility: "irreversable"}
	if err := typo.Validate(); err == nil {
		t.Error("a misspelled reversibility class validated, so the rule would load and never fire")
	}
}
