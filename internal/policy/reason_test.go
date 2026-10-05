package policy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

// TestReasonRequirementComposesAcrossRules pins that the reason a run's decision must carry is the
// strictest asked by any rule that would hold it, whatever order the rules are in, and that a deny
// rule, a rule that does not match, and a Rego policy that does not hold the run ask for nothing.
func TestReasonRequirementComposesAcrossRules(t *testing.T) {
	t.Parallel()
	deploy := &run.Run{Tool: run.ToolBash, Command: "deploy prod"}
	tests := []struct {
		// Policies are the rules in force.
		Policies []*Policy
		// WantRequirement is what the run carries.
		WantRequirement string
	}{{ // Test 0: No rule asks.
		Policies:        []*Policy{{Name: "hold", MaxDestroy: DisabledMaxDestroy}},
		WantRequirement: "",
	}, { // Test 1: One matching rule asks on denials.
		Policies: []*Policy{{Name: "hold", MaxDestroy: DisabledMaxDestroy,
			RequireReason: "denials"}},
		WantRequirement: "denials",
	}, { // Test 2: The stricter of two matching rules wins, listed second.
		Policies: []*Policy{
			{Name: "a", MaxDestroy: DisabledMaxDestroy, RequireReason: "denials"},
			{Name: "b", MaxDestroy: DisabledMaxDestroy, RequireReason: "always"},
		},
		WantRequirement: "always",
	}, { // Test 3: A rule that does not match the run asks nothing of it.
		Policies: []*Policy{{Name: "tf", Tool: run.ToolTerraform, MaxDestroy: DisabledMaxDestroy,
			RequireReason: "always"}},
		WantRequirement: "",
	}, { // Test 4: A deny rule holds nothing, so it asks nothing.
		Policies: []*Policy{{Name: "no", Effect: EffectDeny, MaxDestroy: DisabledMaxDestroy,
			RequireReason: "always"}},
		WantRequirement: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := ReasonRequirement(test.Policies, deploy)
			if diff := cmp.Diff(test.WantRequirement, got); diff != "" {
				t.Errorf("ReasonRequirement() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestARegoPolicyAsksForAReasonWhenItHolds pins that a reason requirement set beside a Rego policy
// applies to the runs that policy holds and to no other.
func TestARegoPolicyAsksForAReasonWhenItHolds(t *testing.T) {
	t.Parallel()
	p := regoPolicy(t, "prod", `hold contains "production" if contains(input.run.command, "prod")`)
	p.RequireReason = "always"
	if got := ReasonRequirement([]*Policy{p}, &run.Run{Tool: run.ToolBash,
		Command: "deploy prod"}); got != "always" {
		t.Errorf("held run requirement = %q, want always", got)
	}
	if got := ReasonRequirement([]*Policy{p}, &run.Run{Tool: run.ToolBash,
		Command: "deploy staging"}); got != "" {
		t.Errorf("unheld run requirement = %q, want none", got)
	}
}

// TestRequireReasonIsValidatedWhereItIsWritten pins that a misspelled requirement is refused rather
// than quietly asking for nothing.
func TestRequireReasonIsValidatedWhereItIsWritten(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Requirement is what the rule sets.
		Requirement string
		// WantValid is whether the rule is accepted.
		WantValid bool
	}{{ // Test 0: None.
		Requirement: "", WantValid: true,
	}, { // Test 1: Denials.
		Requirement: "denials", WantValid: true,
	}, { // Test 2: Always.
		Requirement: "always", WantValid: true,
	}, { // Test 3: A value outside the set is refused.
		Requirement: "approvals", WantValid: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := (&Policy{Name: "x", MaxDestroy: DisabledMaxDestroy,
				RequireReason: test.Requirement}).Validate()
			if (err == nil) != test.WantValid {
				t.Errorf("Validate() error = %v, want valid %v", err, test.WantValid)
			}
		})
	}
}

// TestThePolicyFileCarriesRequireReason pins that the file an operator reviews can set the
// requirement, that a typo in it fails the load, and that the rules in force describe it.
func TestThePolicyFileCarriesRequireReason(t *testing.T) {
	t.Parallel()
	path := reasonPolicyFile(t, "policies:\n  - name: prod destroy\n    command_contains: destroy\n"+
		"    require_reason: always\n")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	list, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].RequireReason != "always" {
		t.Fatalf("loaded = %+v, want one rule asking for a reason always", list)
	}
	set := InForce(list)
	if len(set.Rules) != 1 || !strings.Contains(set.Rules[0], "with a stated reason") {
		t.Errorf("rules in force = %v, want the requirement described", set.Rules)
	}
	bad := reasonPolicyFile(t, "policies:\n  - name: typo\n    require_reason: yes-please\n")
	badStore, err := NewFileStore(bad)
	if err == nil {
		_, err = badStore.List(context.Background())
	}
	if err == nil {
		t.Error("a policy file with an unknown require_reason loaded, so the rule asks for nothing")
	}
}

// reasonPolicyFile writes a policy file and returns its path.
func reasonPolicyFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policies.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}
