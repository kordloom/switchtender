package policy_test

import (
	"testing"

	"github.com/dcadolph/railwarden/internal/policy"
	"github.com/dcadolph/railwarden/internal/policytest"
	"github.com/dcadolph/railwarden/internal/run"
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

// TestRequires confirms any matching policy in a set requires approval.
func TestRequires(t *testing.T) {
	t.Parallel()
	policies := []*policy.Policy{
		{Tool: "terraform", CommandContains: "destroy"},
		{InventoryID: "inv_prod"},
	}
	if !policy.Requires(policies, &run.Run{InventoryID: "inv_prod"}) {
		t.Error("a run targeting inv_prod should require approval")
	}
	if policy.Requires(policies, &run.Run{Tool: "bash", Command: "echo hi"}) {
		t.Error("an unrelated run should not require approval")
	}
}
