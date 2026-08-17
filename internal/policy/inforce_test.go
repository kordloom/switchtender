package policy_test

import (
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestInForceDescribesTheRuleSet covers the half of the boundary that had no evidence behind it. The
// chain proves what was stopped: a held run names the rule that held it, and the decision that released
// it is signed. Nothing recorded what the rules were for a run that sailed through, so "no rule stopped
// this" and "no rule existed" were the same record, and an operator who deleted a gate an hour before a
// change left nothing an auditor could see. A run now carries a digest of the rule set in force when it
// was submitted, and the labels of those rules, so an auditor can tell those two states apart and see
// when the set changed between two runs.
func TestInForceDescribesTheRuleSet(t *testing.T) {
	t.Parallel()
	gate := &policy.Policy{
		ID: "pol_1", Name: "prod terraform destroy", Tool: run.ToolTerraform, MaxDestroy: 2,
	}
	deny := &policy.Policy{
		ID: "pol_2", Name: "agents never drop databases", ActorKind: policy.ActorKindAgent,
		CommandContains: "drop database", Effect: policy.EffectDeny, MaxDestroy: -1,
	}

	// Test 0: An empty rule set is described as empty rather than as unknown. "There were no rules" is
	// a fact an auditor needs, and it is the fact that used to be indistinguishable from silence.
	empty := policy.InForce(nil)
	if empty.Digest == "" {
		t.Error("an install with no policies produced no digest, so a run under no rules is " +
			"indistinguishable from a run whose rules were never recorded")
	}
	if empty.Count != 0 || len(empty.Rules) != 0 {
		t.Errorf("empty set = %+v, want no rules", empty)
	}

	// Test 1: A set is described by its rules, in a stable order, with what each does.
	got := policy.InForce([]*policy.Policy{deny, gate})
	if got.Count != 2 {
		t.Errorf("count = %d, want 2", got.Count)
	}
	joined := strings.Join(got.Rules, "|")
	if !strings.Contains(joined, "prod terraform destroy") || !strings.Contains(joined, "agents never drop databases") {
		t.Errorf("rules = %v, want both rules named", got.Rules)
	}
	if !strings.Contains(joined, "denies") {
		t.Errorf("rules = %v, want a deny rule to say so: the effect is the whole point of the rule",
			got.Rules)
	}

	// Test 2: The digest is over the set, not the order it was listed in, so two servers reading the
	// same policies agree and a reordered list is not reported as a change.
	reordered := policy.InForce([]*policy.Policy{gate, deny})
	if reordered.Digest != got.Digest {
		t.Errorf("digest changed when the list was reordered: %s then %s", got.Digest, reordered.Digest)
	}

	// Test 3: A changed rule changes the digest, which is what makes "the rules were different when
	// that run went through" visible.
	loosened := *gate
	loosened.MaxDestroy = 50
	changed := policy.InForce([]*policy.Policy{deny, &loosened})
	if changed.Digest == got.Digest {
		t.Error("raising a destroy threshold did not change the digest, so a gate loosened before a " +
			"change leaves no trace")
	}

	// Test 4: Removing a rule changes it too, which is the case an auditor is really asking about.
	fewer := policy.InForce([]*policy.Policy{gate})
	if fewer.Digest == got.Digest {
		t.Error("deleting a rule did not change the digest")
	}
}
