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
	}, { // Test 22: A dry run whose playbook forces real tasks is not excluded.
		Name: "forcing dry run not excluded", Policy: policy.Policy{Tool: "ansible", ExcludeDryRun: true},
		Run: run.Run{Playbook: "site.yml", DryRun: true, DryRunScans: []run.DryRunScan{{
			Tool: "ansible", Findings: []string{`site.yml: task "Restart web" sets check_mode to false`},
		}}},
		Want: true,
	}, { // Test 23: A clean Ansible dry run is still excluded.
		Name:   "clean ansible dry run excluded",
		Policy: policy.Policy{Tool: "ansible", ExcludeDryRun: true},
		Run:    run.Run{Playbook: "site.yml", DryRun: true}, Want: false,
	}, { // Test 24: A forcing dry run is graded as the change it is, so it can meet a risk floor.
		Name: "forcing dry run meets a risk floor", Policy: policy.Policy{MinRisk: run.RiskMedium},
		Run: run.Run{Playbook: "site.yml", DryRun: true, DryRunScans: []run.DryRunScan{{
			Tool: "ansible", Findings: []string{`site.yml: task "Restart web" sets check_mode to false`},
		}}},
		Want: true,
	}, { // Test 25: A plan whose configuration runs a program is not excluded.
		Name:   "plan running a program not excluded",
		Policy: policy.Policy{Tool: "terraform", ExcludeDryRun: true},
		Run: run.Run{Tool: "terraform", Command: "infra", DryRun: true,
			DryRunScans: []run.DryRunScan{{Tool: "terraform",
				Findings: []string{"data.external.x runs a program during plan (main.tf line 1)"}}}},
		Want: true,
	}, { // Test 26: A plan whose configuration could not be read in full is not excluded either.
		Name:   "plan read incompletely not excluded",
		Policy: policy.Policy{Tool: "opentofu", ExcludeDryRun: true},
		Run: run.Run{Tool: "opentofu", Command: "infra", DryRun: true,
			DryRunScans: []run.DryRunScan{{Tool: "opentofu",
				Unread: []string{`module.vpc from "acme/vpc/aws" (not downloaded here)`}}}},
		Want: true,
	}, { // Test 27: A plan whose configuration was read in full and runs nothing is excluded.
		Name:   "change-free plan excluded",
		Policy: policy.Policy{Tool: "terraform", ExcludeDryRun: true},
		Run: run.Run{Tool: "terraform", Command: "infra", DryRun: true,
			DryRunScans: []run.DryRunScan{{Tool: "terraform", Inputs: []string{"infra/main.tf"}}}},
		Want: false,
	}, { // Test 28: A plan that runs a program meets a risk floor, graded as the apply it may be.
		Name: "plan running a program meets a risk floor", Policy: policy.Policy{MinRisk: run.RiskMedium},
		Run: run.Run{Tool: "terraform", Command: "infra", DryRun: true,
			DryRunScans: []run.DryRunScan{{Tool: "terraform",
				Findings: []string{"data.external.x runs a program during plan (main.tf line 1)"}}}},
		Want: true,
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
// run gates it, a plan-content policy that does not match does not, and any rule that would hold a
// terraform or opentofu apply plans it first, so the approval binds the plan that applies, except
// for a run its submission asked to hold, a run a decision already released, and a workflow step.
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
	}, { // Test 1: A blanket policy that would hold the apply plans it first.
		Name:     "blanket hold plans first",
		Policies: []*policy.Policy{{Tool: "terraform", MaxDestroy: policy.DisabledMaxDestroy}},
		Run:      run.Run{Tool: "terraform", Command: "infra"}, Want: true,
	}, { // Test 2: A plan-content policy that does not match does not gate.
		Name:     "non-matching plan-content",
		Policies: []*policy.Policy{{Tool: "opentofu", MaxDestroy: 3}},
		Run:      run.Run{Tool: "terraform", Command: "infra"}, Want: false,
	}, { // Test 3: No policies, no gate.
		Name: "no policies", Policies: nil, Run: run.Run{Tool: "terraform"}, Want: false,
	}, { // Test 4: An irreversible floor is reached only through the plan, so the apply plans first.
		Name: "irreversible floor plans first",
		Policies: []*policy.Policy{{Tool: "terraform", Reversibility: run.Irreversible,
			MaxDestroy: policy.DisabledMaxDestroy}},
		Run: run.Run{Tool: "terraform", Command: "infra"}, Want: true,
	}, { // Test 5: So is a high risk floor, since an apply is medium until its plan says more.
		Name:     "high risk floor plans first",
		Policies: []*policy.Policy{{MinRisk: run.RiskHigh, MaxDestroy: policy.DisabledMaxDestroy}},
		Run:      run.Run{Tool: "opentofu", Command: "infra"}, Want: true,
	}, { // Test 6: A floor the apply already meets would hold it, so it plans first too.
		Name: "floor already met",
		Policies: []*policy.Policy{{Reversibility: run.ReversibleCostly,
			MaxDestroy: policy.DisabledMaxDestroy}},
		Run: run.Run{Tool: "terraform", Command: "infra"}, Want: true,
	}, { // Test 7: A floor rule scoped to another tool leaves the apply alone.
		Name: "floor for another tool",
		Policies: []*policy.Policy{{Tool: "ansible", Reversibility: run.Irreversible,
			MaxDestroy: policy.DisabledMaxDestroy}},
		Run: run.Run{Tool: "terraform", Command: "infra"}, Want: false,
	}, { // Test 8: A dry run changes nothing, so no plan is needed to grade it.
		Name: "dry run",
		Policies: []*policy.Policy{{Reversibility: run.Irreversible,
			MaxDestroy: policy.DisabledMaxDestroy}},
		Run: run.Run{Tool: "terraform", Command: "infra", DryRun: true}, Want: false,
	}, { // Test 9: A proposed apply already carries its plan and never gates again.
		Name: "proposed apply",
		Policies: []*policy.Policy{{Reversibility: run.Irreversible,
			MaxDestroy: policy.DisabledMaxDestroy}},
		Run: run.Run{Tool: "terraform", Command: "infra", ProposedFrom: "run_plan"}, Want: false,
	}, { // Test 10: An Ansible run has no plan to read, so a floor never sends it to one.
		Name: "ansible run",
		Policies: []*policy.Policy{{Reversibility: run.Irreversible,
			MaxDestroy: policy.DisabledMaxDestroy}},
		Run: run.Run{Tool: "ansible", Playbook: "site.yml"}, Want: false,
	}, { // Test 11: A deny rule with a floor plans first too, so a destroying apply is refused.
		Name: "deny floor plans first",
		Policies: []*policy.Policy{{Effect: policy.EffectDeny, Reversibility: run.Irreversible,
			MaxDestroy: policy.DisabledMaxDestroy}},
		Run: run.Run{Tool: "terraform", Command: "infra"}, Want: true,
	}, { // Test 12: An apply its submission asked to hold keeps that hold.
		Name:     "held at submission",
		Policies: []*policy.Policy{{Tool: "terraform", MaxDestroy: policy.DisabledMaxDestroy}},
		Run:      run.Run{Tool: "terraform", Command: "infra", Status: run.StatusPendingApproval},
		Want:     false,
	}, { // Test 13: An apply a decision already released is not held again.
		Name:     "already decided",
		Policies: []*policy.Policy{{Tool: "terraform", MaxDestroy: policy.DisabledMaxDestroy}},
		Run:      run.Run{Tool: "terraform", Command: "infra", DecisionID: "dec_1"}, Want: false,
	}, { // Test 14: A workflow step is governed by its workflow's approval.
		Name:     "workflow step",
		Policies: []*policy.Policy{{Tool: "terraform", MaxDestroy: policy.DisabledMaxDestroy}},
		Run:      run.Run{Tool: "terraform", Command: "infra", ParentID: new("run_parent")}, Want: false,
	}, { // Test 15: A deny rule alone holds nothing, so it plans nothing first.
		Name: "deny without a floor",
		Policies: []*policy.Policy{{Effect: policy.EffectDeny, Tool: "terraform",
			MaxDestroy: policy.DisabledMaxDestroy}},
		Run: run.Run{Tool: "terraform", Command: "infra"}, Want: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
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

// TestExceedingDistinct confirms a held apply needs a second person exactly when a rule its destroy
// count exceeds asks for one. Too eager and a lone admin can never release an apply no rule
// reserved for two people; too lax and separation of duties drops silently. The last two rows pin
// the reason the function exists: the strictest exceeded rule decides, whatever the list order.
func TestExceedingDistinct(t *testing.T) {
	t.Parallel()
	strict := func(limit int) *policy.Policy {
		return &policy.Policy{Tool: "terraform", MaxDestroy: limit, RequireDistinctApprover: true}
	}
	tests := []struct {
		Name     string
		Policies []*policy.Policy
		Run      run.Run
		Destroys int
		Want     bool
	}{{ // Test 0: A distinct-approver rule the count exceeds demands a second person.
		Name:     "exceeded strict rule",
		Policies: []*policy.Policy{strict(2)},
		Run:      run.Run{Tool: "terraform"}, Destroys: 3, Want: true,
	}, { // Test 1: At the limit nothing is exceeded, so no second person is demanded.
		Name:     "at the limit",
		Policies: []*policy.Policy{strict(2)},
		Run:      run.Run{Tool: "terraform"}, Destroys: 2, Want: false,
	}, { // Test 2: An exceeded limit that asks for no second person demands none.
		Name:     "exceeded loose rule",
		Policies: []*policy.Policy{{Tool: "terraform", MaxDestroy: 2}},
		Run:      run.Run{Tool: "terraform"}, Destroys: 3, Want: false,
	}, { // Test 3: A strict rule scoped to another tool demands nothing of this run.
		Name:     "other tool",
		Policies: []*policy.Policy{{Tool: "opentofu", MaxDestroy: 0, RequireDistinctApprover: true}},
		Run:      run.Run{Tool: "terraform"}, Destroys: 5, Want: false,
	}, { // Test 4: A disabled limit never demands a second person, however large the plan.
		Name:     "disabled limit",
		Policies: []*policy.Policy{strict(policy.DisabledMaxDestroy)},
		Run:      run.Run{Tool: "terraform"}, Destroys: 99, Want: false,
	}, { // Test 5: A zero limit demands a second person for any destroy.
		Name:     "zero limit",
		Policies: []*policy.Policy{strict(0)},
		Run:      run.Run{Tool: "terraform"}, Destroys: 1, Want: true,
	}, { // Test 6: A loose rule listed ahead of an exceeded strict one does not drop the second person.
		Name:     "strict rule behind a loose one",
		Policies: []*policy.Policy{{Tool: "terraform", MaxDestroy: 1}, strict(2)},
		Run:      run.Run{Tool: "terraform"}, Destroys: 3, Want: true,
	}, { // Test 7: A strict rule the count stays under demands nothing beside an exceeded loose one.
		Name:     "strict rule not exceeded",
		Policies: []*policy.Policy{{Tool: "terraform", MaxDestroy: 1}, strict(5)},
		Run:      run.Run{Tool: "terraform"}, Destroys: 3, Want: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := test.Run
			if got := policy.ExceedingDistinct(test.Policies, &r, test.Destroys); got != test.Want {
				t.Errorf("%s: ExceedingDistinct() = %v, want %v", test.Name, got, test.Want)
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
	// The built-in agent hold covers the agent's run, but no stored rule may: a deny rule never
	// doubles as an approval rule, and the dispatcher refuses the run before it asks for a hold.
	if got := policy.Requiring(policies, agentDrop); got != nil && !policy.IsAgentDefault(got) {
		t.Errorf("Requiring(agent drop) = %v, want no stored rule: a denied run is refused, not "+
			"held", got)
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
