package policy

import (
	"fmt"
	"testing"
)

// TestAdvancedCoversEveryDocumentedFullEngineFeature holds this package's definition of the full
// policy engine against what the product sells.
//
// Advanced is the definition the licensing gate reads on every path that can introduce a policy: the
// create and update handlers, and the --policy-file load at startup that an install managing policy
// as code actually uses. Its own comment claimed to be the one definition every enforcement point
// shares, and it was not: two handlers carried their own copies and this was a third, and none of
// the three tested for actor scoping while the licensing terms, the API reference, the FAQ, and the
// comment on license.FeaturePolicyFull all sold it as Team.
//
// The result was a rule scoped to agents as a class running free, through the API and through the
// policy file. Actor scoping is the criterion that turns a blanket hold into an authorization
// boundary around a machine principal, which is the capability being sold, so it is the one that
// most needed the gate.
func TestAdvancedCoversEveryDocumentedFullEngineFeature(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Policy   Policy
		WantFull bool
	}{{ // Test 0: The one plain require-approval rule Community holds.
		Name: "a plain require-approval rule", Policy: Policy{Name: "hold"}, WantFull: false,
	}, { // Test 1: Narrowing by tool does not reach the full engine.
		Name: "scoped to a tool", Policy: Policy{Name: "ansible", Tool: "ansible"}, WantFull: false,
	}, { // Test 2: Narrowing by inventory does not either.
		Name:   "scoped to an inventory",
		Policy: Policy{Name: "prod", InventoryID: "inv_1"}, WantFull: false,
	}, { // Test 3: A deny effect refuses the submission outright.
		Name: "a deny rule", Policy: Policy{Name: "no", Effect: EffectDeny}, WantFull: true,
	}, { // Test 4: A risk floor makes the advisory grade enforceable.
		Name: "a risk floor", Policy: Policy{Name: "risky", MinRisk: "high"}, WantFull: true,
	}, { // Test 5: Separation of duties refuses the requester's own decision.
		Name:   "distinct approver",
		Policy: Policy{Name: "sod", RequireDistinctApprover: true}, WantFull: true,
	}, { // Test 6: Scoping to agents as a class is the machine-principal boundary.
		Name:   "scoped to agents as a class",
		Policy: Policy{Name: "agents", ActorKind: ActorKindAgent}, WantFull: true,
	}, { // Test 7: Scoping to people as a class is the same criterion.
		Name:   "scoped to humans as a class",
		Policy: Policy{Name: "people", ActorKind: ActorKindHuman}, WantFull: true,
	}, { // Test 8: Scoping to one named principal is the same criterion again.
		Name:   "scoped to one named actor",
		Policy: Policy{Name: "bot", Actor: "deploy-agent"}, WantFull: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := test.Policy.Advanced(); got != test.WantFull {
				t.Errorf("%s: Advanced() = %v, want %v. The gate and the tier the documentation "+
					"sells have to agree in both directions: a capability sold as Team that answers "+
					"on Community is given away, and one sold as Community that refuses is a broken "+
					"free tier", test.Name, got, test.WantFull)
			}
		})
	}
}
