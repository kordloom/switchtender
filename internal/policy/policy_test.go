package policy_test

import (
	"fmt"
	"testing"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/policytest"
	"github.com/kordloom/switchtender/internal/run"
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
	}, { // Test 11: An agent-scoped rule matches an agent's run.
		Name: "agent kind match", Policy: policy.Policy{ActorKind: policy.ActorKindAgent},
		Run: run.Run{ActorType: "agent"}, Want: true,
	}, { // Test 12: An agent-scoped rule leaves a person's run alone.
		Name: "agent kind mismatch", Policy: policy.Policy{ActorKind: policy.ActorKindAgent},
		Run: run.Run{ActorType: "session"}, Want: false,
	}, { // Test 13: A human-scoped rule matches a signed-in person.
		Name: "human kind session", Policy: policy.Policy{ActorKind: policy.ActorKindHuman},
		Run: run.Run{ActorType: "session"}, Want: true,
	}, { // Test 14: A human-scoped rule matches an owner-held token.
		Name: "human kind token", Policy: policy.Policy{ActorKind: policy.ActorKindHuman},
		Run: run.Run{ActorType: "token"}, Want: true,
	}, { // Test 15: A webhook run is neither kind, so an actor-scoped rule never fires on it.
		Name: "webhook is neither kind", Policy: policy.Policy{ActorKind: policy.ActorKindHuman},
		Run: run.Run{ActorType: "webhook"}, Want: false,
	}, { // Test 16: A run with no recorded actor type matches no named kind.
		Name: "unknown actor unmatched", Policy: policy.Policy{ActorKind: policy.ActorKindAgent},
		Run: run.Run{}, Want: false,
	}, { // Test 17: A named-actor rule binds to exactly that principal.
		Name: "actor name match", Policy: policy.Policy{Actor: "prod-remediator"},
		Run: run.Run{Actor: "prod-remediator", ActorType: "agent"}, Want: true,
	}, { // Test 18: A named-actor rule leaves every other principal alone.
		Name: "actor name mismatch", Policy: policy.Policy{Actor: "prod-remediator"},
		Run: run.Run{Actor: "operator-jane", ActorType: "session"}, Want: false,
	}, { // Test 19: A misspelled kind matches nothing rather than everything.
		Name: "unknown kind matches nothing", Policy: policy.Policy{ActorKind: "robot"},
		Run: run.Run{ActorType: "agent"}, Want: false,
	}, { // Test 20: A destructive command grades high and meets a high floor.
		Name: "min risk met", Policy: policy.Policy{MinRisk: run.RiskHigh},
		Run: run.Run{Tool: "bash", Command: "rm -rf /var/data"}, Want: true,
	}, { // Test 21: A dry run grades low and stays under a high floor.
		Name: "min risk unmet", Policy: policy.Policy{MinRisk: run.RiskHigh},
		Run: run.Run{Tool: "bash", Command: "echo ok", DryRun: true}, Want: false,
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

// TestRequireDistinctIsOrderIndependent pins that separation of duties composes by OR across every
// matching rule.
//
// The flag used to be copied from whichever matching rule came first, so a stricter rule later in
// the list silently did nothing: two admins could reorder policies and lower a control without
// either of them touching the control itself. If any rule covering the run demands a second
// person, the run demands a second person, in either order, and a deny rule's flag stays out of it.
func TestRequireDistinctIsOrderIndependent(t *testing.T) {
	t.Parallel()
	lax := &policy.Policy{InventoryID: "inv_prod", MaxDestroy: policy.DisabledMaxDestroy}
	strict := &policy.Policy{
		Tool: "terraform", MaxDestroy: policy.DisabledMaxDestroy, RequireDistinctApprover: true,
	}
	r := &run.Run{Tool: "terraform", InventoryID: "inv_prod"}

	if !policy.RequireDistinct([]*policy.Policy{lax, strict}, r) {
		t.Error("the stricter rule listed second was dropped, which lowers separation of duties by ordering")
	}
	if !policy.RequireDistinct([]*policy.Policy{strict, lax}, r) {
		t.Error("the stricter rule listed first should hold too")
	}
	if policy.RequireDistinct([]*policy.Policy{lax}, r) {
		t.Error("no matching rule demands a second person, so none is required")
	}
	// A plan-content rule's demand counts: the plan gate holds under the same requirement.
	plan := &policy.Policy{Tool: "terraform", MaxDestroy: 2, RequireDistinctApprover: true}
	if !policy.RequireDistinct([]*policy.Policy{lax, plan}, r) {
		t.Error("a plan-content rule demanding a second person was ignored")
	}
	// A non-matching strict rule stays out of it.
	other := &policy.Policy{Tool: "bash", MaxDestroy: policy.DisabledMaxDestroy, RequireDistinctApprover: true}
	if policy.RequireDistinct([]*policy.Policy{lax, other}, r) {
		t.Error("a rule that does not cover this run must not add requirements to it")
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

// TestDenying confirms a deny policy is found for the runs it matches, that a deny rule never
// doubles as an approval rule, and that Requiring skips it so a denied run is refused rather than
// parked in front of an approver.
func TestDenying(t *testing.T) {
	t.Parallel()
	deny := &policy.Policy{
		ID: "pol_deny", Name: "no agent drops", ActorKind: policy.ActorKindAgent,
		CommandContains: "drop database", Effect: policy.EffectDeny,
		MaxDestroy: policy.DisabledMaxDestroy,
	}
	hold := &policy.Policy{
		ID: "pol_hold", Name: "hold tf", Tool: "terraform", MaxDestroy: policy.DisabledMaxDestroy,
	}
	policies := []*policy.Policy{deny, hold}

	agentDrop := &run.Run{Tool: "bash", Command: "psql -c 'drop database prod'", ActorType: "agent"}
	if got := policy.Denying(policies, agentDrop); got != deny {
		t.Errorf("Denying(agent drop) = %v, want the deny policy", got)
	}
	if got := policy.Requiring(policies, agentDrop); got != nil {
		t.Errorf("Requiring(agent drop) = %v, want nil: a denied run is refused, not held", got)
	}

	humanDrop := &run.Run{Tool: "bash", Command: "psql -c 'drop database prod'", ActorType: "session"}
	if got := policy.Denying(policies, humanDrop); got != nil {
		t.Errorf("Denying(human drop) = %v, want nil: the rule is scoped to agents", got)
	}

	tfRun := &run.Run{Tool: "terraform", Command: "terraform apply", ActorType: "agent"}
	if got := policy.Denying(policies, tfRun); got != nil {
		t.Errorf("Denying(tf) = %v, want nil", got)
	}
	if got := policy.Requiring(policies, tfRun); got != hold {
		t.Errorf("Requiring(tf) = %v, want the hold policy", got)
	}
}

// TestPolicyValidate confirms the vocabulary is checked where a rule is written, so a typo is an
// error rather than a rule that silently matches nothing or gates nothing.
func TestPolicyValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Policy  policy.Policy
		WantErr bool
	}{{ // Test 0: An empty policy is valid.
		Name: "empty", Policy: policy.Policy{MaxDestroy: policy.DisabledMaxDestroy}, WantErr: false,
	}, { // Test 1: The full valid vocabulary.
		Name: "valid full", Policy: policy.Policy{
			ActorKind: policy.ActorKindAgent, MinRisk: run.RiskHigh,
			Effect: policy.EffectDeny, MaxDestroy: policy.DisabledMaxDestroy,
		}, WantErr: false,
	}, { // Test 2: An unknown effect is refused.
		Name: "bad effect", Policy: policy.Policy{Effect: "refuse",
			MaxDestroy: policy.DisabledMaxDestroy}, WantErr: true,
	}, { // Test 3: An unknown actor kind is refused.
		Name: "bad kind", Policy: policy.Policy{ActorKind: "robot",
			MaxDestroy: policy.DisabledMaxDestroy}, WantErr: true,
	}, { // Test 4: An unknown risk level is refused.
		Name: "bad risk", Policy: policy.Policy{MinRisk: "severe",
			MaxDestroy: policy.DisabledMaxDestroy}, WantErr: true,
	}, { // Test 5: Deny cannot combine with a plan-content threshold.
		Name: "deny with max destroy", Policy: policy.Policy{Effect: policy.EffectDeny,
			MaxDestroy: 0}, WantErr: true,
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			p := test.Policy
			if err := p.Validate(); (err != nil) != test.WantErr {
				t.Errorf("Validate() error = %v, want error %v", err, test.WantErr)
			}
		})
	}
}
