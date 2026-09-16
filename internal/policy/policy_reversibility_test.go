package policy

import (
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestAPolicyCanGateOnWhatCannotBeUndone covers the rule this criterion exists to let somebody
// write: a change nobody can take back needs a person to agree to it first.
//
// Before it, the nearest expressible rule was a risk floor, and risk answers a different question.
// A high-risk floor holds a fleet restart, which undoes itself, and lets a table drop through if
// the command reads calmly enough. The two runs below are exactly that pair.
func TestAPolicyCanGateOnWhatCannotBeUndone(t *testing.T) {
	t.Parallel()
	permanent := &run.Run{Tool: run.ToolBash, Command: "psql -c 'DROP TABLE orders'"}
	recoverable := &run.Run{Tool: run.ToolBash, Command: "shutdown -r now"}

	gate := &Policy{Name: "two eyes on the permanent", Effect: EffectRequireApproval,
		Reversibility: run.Irreversible}
	if err := gate.Validate(); err != nil {
		t.Fatalf("policy did not validate: %v", err)
	}
	if !gate.Matches(permanent) {
		t.Error("the dropped table did not match an irreversible rule, so nothing holds it")
	}
	if gate.Matches(recoverable) {
		t.Error("the restart matched an irreversible rule; a gate that fires on recoverable work " +
			"is one an operator switches off, which is how the permanent changes get through")
	}

	// A typo is refused when the file loads, not silently at match time. Left to match time the
	// rule loads, lists, and never fires.
	typo := &Policy{Name: "typo", Effect: EffectRequireApproval, Reversibility: "irreversable"}
	if err := typo.Validate(); err == nil {
		t.Error("a misspelled reversibility class validated, so the rule would load and never fire")
	}
}
