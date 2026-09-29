package server

import (
	"fmt"
	"testing"

	"github.com/kordloom/switchtender/internal/policy"
)

// TestEveryDocumentedFullEngineFeatureIsGated holds the license gate against what the product says
// it sells.
//
// The licensing terms, the API reference, the FAQ, and the comment on license.FeaturePolicyFull all
// name four things as the full policy engine: deny rules, risk floors, actor scoping, and
// distinct-approver separation of duties. The gate tested for three. A policy scoped to agents as a
// class, or to one named actor, was created on a Community install with no license and no refusal,
// so the one capability that turns a hold into an authorization boundary around a machine principal
// was the one being given away.
//
// Nothing held the gate to the sentence describing it, and the create and update handlers each
// carried their own copy of the condition, so the drift had two places to hide.
func TestEveryDocumentedFullEngineFeatureIsGated(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Request  createPolicyRequest
		WantFull bool
	}{{ // Test 0: A plain require-approval rule is the one Community holds.
		Name:    "a plain require-approval rule",
		Request: createPolicyRequest{Name: "hold everything"}, WantFull: false,
	}, { // Test 1: Matching on a tool narrows the rule without reaching the engine.
		Name:    "scoped to a tool",
		Request: createPolicyRequest{Name: "ansible", Tool: "ansible"}, WantFull: false,
	}, { // Test 2: A deny rule refuses the submission outright.
		Name:    "a deny rule",
		Request: createPolicyRequest{Name: "no", Effect: policy.EffectDeny}, WantFull: true,
	}, { // Test 3: A risk floor turns the advisory grade into an enforceable criterion.
		Name:    "a risk floor",
		Request: createPolicyRequest{Name: "risky", MinRisk: "high"}, WantFull: true,
	}, { // Test 4: Separation of duties refuses a decision by the person who asked.
		Name:    "distinct approver",
		Request: createPolicyRequest{Name: "sod", RequireDistinctApprover: true}, WantFull: true,
	}, { // Test 5: Scoping to agents as a class is the machine-principal boundary.
		Name:     "scoped to agents as a class",
		Request:  createPolicyRequest{Name: "agents", ActorKind: policy.ActorKindAgent},
		WantFull: true,
	}, { // Test 6: Scoping to people as a class is the same criterion.
		Name:     "scoped to humans as a class",
		Request:  createPolicyRequest{Name: "people", ActorKind: policy.ActorKindHuman},
		WantFull: true,
	}, { // Test 7: Scoping to one named principal is the same criterion again.
		Name:    "scoped to one named actor",
		Request: createPolicyRequest{Name: "just-the-bot", Actor: "deploy-agent"}, WantFull: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := usesFullPolicyEngine(test.Request); got != test.WantFull {
				t.Errorf("%s: usesFullPolicyEngine() = %v, want %v. The gate and the tier the "+
					"documentation sells have to agree, in both directions: a capability sold as "+
					"Team that answers on Community is given away, and one sold as Community that "+
					"refuses is a broken free tier", test.Name, got, test.WantFull)
			}
		})
	}
}
