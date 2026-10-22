package server

import (
	"fmt"
	"testing"

	"github.com/kordloom/switchtender/internal/policy"
)

// TestEveryDocumentedPaidRuleIsGatedAtItsTier holds the license gate against what the product says
// it sells.
//
// Deny rules and the risk and reversibility floors are Pro. Separation of duties and actor, actor
// kind, and account scoping are Team. The gate once tested for three of the four things the terms
// named as the full engine, and a policy scoped to agents as a class was created on a Community
// install with no license and no refusal, so the one capability that turns a hold into an
// authorization boundary around a machine principal was the one being given away.
//
// Nothing held the gate to the sentence describing it, and the create and update handlers each
// carried their own copy of the condition, so the drift had two places to hide. Both now build the
// rule with requestRules and ask policy.AllowRules, and this holds what requestRules carries.
func TestEveryDocumentedPaidRuleIsGatedAtItsTier(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case in a failure.
		Name string
		// Request is the create or update body.
		Request createPolicyRequest
		// WantPro is whether the rule it describes uses a Pro rule.
		WantPro bool
		// WantTeam is whether it uses a Team rule.
		WantTeam bool
	}{{ // Test 0: A plain require-approval rule is the one Community holds.
		Name:    "a plain require-approval rule",
		Request: createPolicyRequest{Name: "hold everything"},
	}, { // Test 1: Matching on a tool narrows the rule without reaching a paid tier.
		Name:    "scoped to a tool",
		Request: createPolicyRequest{Name: "ansible", Tool: "ansible"},
	}, { // Test 2: A deny rule refuses the submission outright.
		Name:    "a deny rule",
		Request: createPolicyRequest{Name: "no", Effect: policy.EffectDeny}, WantPro: true,
	}, { // Test 3: A risk floor turns the advisory grade into an enforceable criterion.
		Name:    "a risk floor",
		Request: createPolicyRequest{Name: "risky", MinRisk: "high"}, WantPro: true,
	}, { // Test 4: A reversibility floor holds what cannot be taken back.
		Name:    "a reversibility floor",
		Request: createPolicyRequest{Name: "forever", Reversibility: "irreversible"}, WantPro: true,
	}, { // Test 5: Separation of duties refuses a decision by the person who asked.
		Name:    "distinct approver",
		Request: createPolicyRequest{Name: "sod", RequireDistinctApprover: true}, WantTeam: true,
	}, { // Test 6: Scoping to agents as a class is the machine-principal boundary.
		Name:     "scoped to agents as a class",
		Request:  createPolicyRequest{Name: "agents", ActorKind: policy.ActorKindAgent},
		WantTeam: true,
	}, { // Test 7: Scoping to people as a class is the same criterion.
		Name:     "scoped to humans as a class",
		Request:  createPolicyRequest{Name: "people", ActorKind: policy.ActorKindHuman},
		WantTeam: true,
	}, { // Test 8: Scoping to one named principal is the same criterion again.
		Name:     "scoped to one named actor",
		Request:  createPolicyRequest{Name: "just-the-bot", Actor: "deploy-agent"},
		WantTeam: true,
	}, { // Test 9: Scoping to one account is the same boundary drawn around the account.
		Name:     "scoped to one account",
		Request:  createPolicyRequest{Name: "team a", Account: "team-a"},
		WantTeam: true,
	}, { // Test 10: An exemption naming an account stays free.
		Name:    "an exemption naming an account",
		Request: createPolicyRequest{Name: "ci", Effect: policy.EffectExempt, Account: "team-a"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			p := requestRules(test.Request)
			if got := p.ProRules(); got != test.WantPro {
				t.Errorf("%s: ProRules() = %v, want %v. The gate and the tier the documentation "+
					"sells have to agree, in both directions", test.Name, got, test.WantPro)
			}
			if got := p.TeamRules(); got != test.WantTeam {
				t.Errorf("%s: TeamRules() = %v, want %v. The gate and the tier the documentation "+
					"sells have to agree, in both directions", test.Name, got, test.WantTeam)
			}
		})
	}
}
