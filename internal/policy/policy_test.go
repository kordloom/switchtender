package policy_test

import (
	"fmt"
	"testing"

	"github.com/dcadolph/switchtender/internal/policy"
	"github.com/dcadolph/switchtender/internal/policytest"
	"github.com/dcadolph/switchtender/internal/run"
)

// TestMemStoreContract runs the store contract against the in-memory policy store.
func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	policytest.Contract(t, func() policy.Store { return policy.NewMemStore() })
}

// TestPolicyMatches covers the matcher: an empty policy matches all, each criterion narrows the
// match, a dry run is excluded when asked, and an empty tool normalizes to ansible.
func TestPolicyMatches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name   string
		Policy policy.Policy
		Run    run.Run
		Want   bool
	}{{ // Test 0: An empty policy matches every run.
		Name: "empty matches all", Policy: policy.Policy{}, Run: run.Run{Tool: "bash"}, Want: true,
	}, { // Test 1: Tool matches.
		Name: "tool match", Policy: policy.Policy{Tool: "terraform"}, Run: run.Run{Tool: "terraform"}, Want: true,
	}, { // Test 2: Tool mismatch.
		Name: "tool mismatch", Policy: policy.Policy{Tool: "terraform"}, Run: run.Run{Tool: "bash"}, Want: false,
	}, { // Test 3: An empty run tool normalizes to ansible.
		Name: "tool ansible default", Policy: policy.Policy{Tool: "ansible"}, Run: run.Run{Tool: ""}, Want: true,
	}, { // Test 4: Command substring matches.
		Name: "command contains", Policy: policy.Policy{CommandContains: "destroy"},
		Run: run.Run{Command: "terraform destroy -auto-approve"}, Want: true,
	}, { // Test 5: Command substring absent.
		Name: "command missing", Policy: policy.Policy{CommandContains: "destroy"},
		Run: run.Run{Command: "terraform apply"}, Want: false,
	}, { // Test 6: Inventory matches.
		Name: "inventory match", Policy: policy.Policy{InventoryID: "inv_prod"},
		Run: run.Run{InventoryID: "inv_prod"}, Want: true,
	}, { // Test 7: All criteria must match.
		Name: "combined match", Policy: policy.Policy{Tool: "terraform", CommandContains: "destroy"},
		Run: run.Run{Tool: "terraform", Command: "destroy prod"}, Want: true,
	}, { // Test 8: One criterion off fails the whole match.
		Name: "combined mismatch", Policy: policy.Policy{Tool: "terraform", CommandContains: "destroy"},
		Run: run.Run{Tool: "terraform", Command: "apply"}, Want: false,
	}, { // Test 9: A dry run is excluded when the policy asks.
		Name: "dry run excluded", Policy: policy.Policy{Tool: "terraform", ExcludeDryRun: true},
		Run: run.Run{Tool: "terraform", DryRun: true}, Want: false,
	}, { // Test 10: A real run is not excluded.
		Name: "real run not excluded", Policy: policy.Policy{Tool: "terraform", ExcludeDryRun: true},
		Run: run.Run{Tool: "terraform", DryRun: false}, Want: true,
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			p, r := test.Policy, test.Run
			if got := p.Matches(&r); got != test.Want {
				t.Errorf("Matches() = %v, want %v", got, test.Want)
			}
		})
	}
}

// TestRequires confirms any matching blanket policy in a set requires approval, while a plan-content
// policy is left to the execution gate and never blanket-holds at submission.
func TestRequires(t *testing.T) {
	t.Parallel()
	policies := []*policy.Policy{
		{Tool: "terraform", CommandContains: "destroy", MaxDestroy: policy.DisabledMaxDestroy},
		{InventoryID: "inv_prod", MaxDestroy: policy.DisabledMaxDestroy},
	}
	if !policy.Requires(policies, &run.Run{InventoryID: "inv_prod"}) {
		t.Error("a run targeting inv_prod should require approval")
	}
	if policy.Requires(policies, &run.Run{Tool: "bash", Command: "echo hi"}) {
		t.Error("an unrelated run should not require approval")
	}
	// A plan-content policy is enforced at execution, not blanket-held at submission.
	planContent := []*policy.Policy{{Tool: "terraform", MaxDestroy: 2}}
	if policy.Requires(planContent, &run.Run{Tool: "terraform", Command: "infra"}) {
		t.Error("a plan-content policy should not blanket-hold at submission")
	}
}

// TestPlanGated covers the plan gate scope check: a plan-content policy (MaxDestroy >= 0) matching a
// run gates it, a blanket policy (MaxDestroy < 0) does not, and a plan-content policy that does not
// match does not gate.
func TestPlanGated(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Policies []*policy.Policy
		Run      run.Run
		Want     bool
	}{{ // Test 0: A matching plan-content policy gates the run.
		Name:     "matching plan-content gates",
		Policies: []*policy.Policy{{Tool: "terraform", MaxDestroy: 0}},
		Run:      run.Run{Tool: "terraform", Command: "infra"}, Want: true,
	}, { // Test 1: A blanket policy never gates on plan content.
		Name:     "blanket does not gate",
		Policies: []*policy.Policy{{Tool: "terraform", MaxDestroy: policy.DisabledMaxDestroy}},
		Run:      run.Run{Tool: "terraform", Command: "infra"}, Want: false,
	}, { // Test 2: A plan-content policy that does not match does not gate.
		Name:     "non-matching plan-content",
		Policies: []*policy.Policy{{Tool: "opentofu", MaxDestroy: 3}},
		Run:      run.Run{Tool: "terraform", Command: "infra"}, Want: false,
	}, { // Test 3: No policies, no gate.
		Name: "no policies", Policies: nil, Run: run.Run{Tool: "terraform"}, Want: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := test.Run
			if got := policy.PlanGated(test.Policies, &r); got != test.Want {
				t.Errorf("PlanGated() = %v, want %v", got, test.Want)
			}
		})
	}
}

// TestPlanExceeds covers the plan-content threshold: a matching enabled policy is violated only when
// destroys is over its threshold, a run at the threshold is allowed, a disabled policy never fires,
// and a non-matching policy is ignored.
func TestPlanExceeds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Policies []*policy.Policy
		Run      run.Run
		Destroys int
		Want     bool
	}{{ // Test 0: Over the threshold is a violation.
		Name:     "over threshold",
		Policies: []*policy.Policy{{Tool: "terraform", MaxDestroy: 2}},
		Run:      run.Run{Tool: "terraform"}, Destroys: 3, Want: true,
	}, { // Test 1: At the threshold is allowed.
		Name:     "at threshold",
		Policies: []*policy.Policy{{Tool: "terraform", MaxDestroy: 2}},
		Run:      run.Run{Tool: "terraform"}, Destroys: 2, Want: false,
	}, { // Test 2: Under the threshold is allowed.
		Name:     "under threshold",
		Policies: []*policy.Policy{{Tool: "terraform", MaxDestroy: 2}},
		Run:      run.Run{Tool: "terraform"}, Destroys: 1, Want: false,
	}, { // Test 3: A threshold of zero holds on any destroy.
		Name:     "zero holds any destroy",
		Policies: []*policy.Policy{{Tool: "terraform", MaxDestroy: 0}},
		Run:      run.Run{Tool: "terraform"}, Destroys: 1, Want: true,
	}, { // Test 4: A disabled policy never fires, even on a large plan.
		Name:     "disabled never fires",
		Policies: []*policy.Policy{{Tool: "terraform", MaxDestroy: policy.DisabledMaxDestroy}},
		Run:      run.Run{Tool: "terraform"}, Destroys: 99, Want: false,
	}, { // Test 5: A non-matching policy is ignored.
		Name:     "non-matching ignored",
		Policies: []*policy.Policy{{Tool: "opentofu", MaxDestroy: 0}},
		Run:      run.Run{Tool: "terraform"}, Destroys: 5, Want: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := test.Run
			if got := policy.PlanExceeds(test.Policies, &r, test.Destroys); got != test.Want {
				t.Errorf("PlanExceeds() = %v, want %v", got, test.Want)
			}
		})
	}
}
