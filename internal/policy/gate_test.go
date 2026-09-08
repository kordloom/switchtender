package policy_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// blanket returns a copy of p with the plan-content check disabled, which is what makes it a rule
// enforced at submission rather than one the plan gate owns.
func blanket(p policy.Policy) *policy.Policy {
	p.MaxDestroy = policy.DisabledMaxDestroy
	return &p
}

// TestEveryCriterionMatchesAndRefuses walks each matching dimension a rule can name and proves both
// halves of it: the rule fires on the run it describes and stays off every other run.
//
// This is the approval gate itself. A dimension that matches too widely holds work nobody meant to
// hold, and one that matches too narrowly is a gate an operator believes exists and does not. The
// queue dimension had no test at all, and it is the one that decides which segment of the estate a
// rule covers, so a rule written to hold everything headed for production rested on nothing.
func TestEveryCriterionMatchesAndRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Policy is the rule under test.
		Policy policy.Policy
		// Run is the run offered to it.
		Run run.Run
		// WantMatch is whether the rule must cover that run.
		WantMatch bool
	}{{ // Test 0: A rule naming a queue covers the run routed there.
		Policy: policy.Policy{Queue: "prod"}, Run: run.Run{Queue: "prod"}, WantMatch: true,
	}, { // Test 1: The same rule leaves another queue alone.
		Policy: policy.Policy{Queue: "prod"}, Run: run.Run{Queue: "staging"}, WantMatch: false,
	}, { // Test 2: A queue rule does not fire on a run with no queue at all, so a default-routed run
		// is not swept into a rule written for one segment.
		Policy: policy.Policy{Queue: "prod"}, Run: run.Run{}, WantMatch: false,
	}, { // Test 3: A rule naming no queue covers a queued run, which is the empty-matches-any promise.
		Policy: policy.Policy{}, Run: run.Run{Queue: "prod"}, WantMatch: true,
	}, { // Test 4: Queue matching is exact, not a prefix, so "prod" never reaches "prod-canary".
		Policy: policy.Policy{Queue: "prod"}, Run: run.Run{Queue: "prod-canary"}, WantMatch: false,
	}, { // Test 5: An inventory rule is exact too.
		Policy: policy.Policy{InventoryID: "inv_prod"}, Run: run.Run{InventoryID: "inv_prod_2"},
		WantMatch: false,
	}, { // Test 6: A named actor and a kind must both hold.
		Policy: policy.Policy{Actor: "deploy-bot", ActorKind: policy.ActorKindAgent},
		Run:    run.Run{Actor: "deploy-bot", ActorType: "agent"}, WantMatch: true,
	}, { // Test 7: The right name with the wrong kind does not match, so a rule bound to a machine
		// principal does not fire when a person of the same name asks.
		Policy: policy.Policy{Actor: "deploy-bot", ActorKind: policy.ActorKindAgent},
		Run:    run.Run{Actor: "deploy-bot", ActorType: "session"}, WantMatch: false,
	}, { // Test 8: The command line is matched from the command line, so a person driving the CLI is
		// a human actor.
		Policy: policy.Policy{ActorKind: policy.ActorKindHuman}, Run: run.Run{ActorType: "cli"},
		WantMatch: true,
	}, { // Test 9: A schedule fires under no named kind, so an actor-scoped rule never fires on it.
		Policy: policy.Policy{ActorKind: policy.ActorKindHuman}, Run: run.Run{ActorType: "schedule"},
		WantMatch: false,
	}, { // Test 10: An actor-kind of the wrong case is an unknown kind and matches nothing.
		Policy: policy.Policy{ActorKind: "Agent"}, Run: run.Run{ActorType: "agent"}, WantMatch: false,
	}, { // Test 11: A medium floor covers an infrastructure apply, which grades medium.
		Policy: policy.Policy{MinRisk: run.RiskMedium}, Run: run.Run{Tool: run.ToolTerraform},
		WantMatch: true,
	}, { // Test 12: A medium floor leaves an ordinary shell command, which grades low, alone.
		Policy: policy.Policy{MinRisk: run.RiskMedium}, Run: run.Run{Tool: run.ToolBash, Command: "echo ok"},
		WantMatch: false,
	}, { // Test 13: A low floor covers everything, since every run grades at least low.
		Policy: policy.Policy{MinRisk: run.RiskLow}, Run: run.Run{Tool: run.ToolBash, Command: "echo ok"},
		WantMatch: true,
	}, { // Test 14: Excluding dry runs does not exclude a real run of the same shape.
		Policy: policy.Policy{ExcludeDryRun: true}, Run: run.Run{Tool: run.ToolTerraform},
		WantMatch: true,
	}, { // Test 15: Excluding dry runs beats every other criterion, including a risk floor the dry run
		// would otherwise fail anyway.
		Policy: policy.Policy{ExcludeDryRun: true, MinRisk: run.RiskLow},
		Run:    run.Run{Tool: run.ToolTerraform, DryRun: true}, WantMatch: false,
	}, { // Test 16: An empty command criterion matches a run with no command.
		Policy: policy.Policy{}, Run: run.Run{Command: ""}, WantMatch: true,
	}, { // Test 17: A command criterion is a substring, anchored nowhere.
		Policy: policy.Policy{CommandContains: "destroy"},
		Run:    run.Run{Command: "cd /infra && terraform destroy -auto-approve"}, WantMatch: true,
	}, { // Test 18: A command criterion no run text carries does not match.
		Policy: policy.Policy{CommandContains: "destroy"}, Run: run.Run{Command: ""}, WantMatch: false,
	}, { // Test 19: Unicode in the criterion and the command matches, so a rule written in any script
		// works.
		Policy: policy.Policy{CommandContains: "supprimer-tout"},
		Run:    run.Run{Command: "./scripts/supprimer-tout --oui"}, WantMatch: true,
	}, { // Test 20: Every dimension at once, all satisfied.
		Policy: policy.Policy{
			Tool: run.ToolTerraform, CommandContains: "destroy", InventoryID: "inv_prod",
			Queue: "prod", ActorKind: policy.ActorKindAgent, Actor: "bot", MinRisk: run.RiskHigh,
			ExcludeDryRun: true,
		},
		Run: run.Run{
			Tool: run.ToolTerraform, Command: "terraform destroy -auto-approve",
			InventoryID: "inv_prod", Queue: "prod", ActorType: "agent", Actor: "bot",
		}, WantMatch: true,
	}, { // Test 21: The same rule with one dimension off does not match, which is what narrowing means.
		Policy: policy.Policy{
			Tool: run.ToolTerraform, CommandContains: "destroy", InventoryID: "inv_prod",
			Queue: "prod", ActorKind: policy.ActorKindAgent, Actor: "bot", MinRisk: run.RiskHigh,
			ExcludeDryRun: true,
		},
		Run: run.Run{
			Tool: run.ToolTerraform, Command: "terraform destroy -auto-approve",
			InventoryID: "inv_prod", Queue: "dmz", ActorType: "agent", Actor: "bot",
		}, WantMatch: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			p, r := test.Policy, test.Run
			if got := p.Matches(&r); got != test.WantMatch {
				t.Errorf("Matches() = %v, want %v for policy %+v and run %+v",
					got, test.WantMatch, p, r)
			}
		})
	}
}

// TestAnEmptyRuleMatchesEveryRunShape pins the documented promise that an empty criterion matches any
// value, one field at a time.
//
// A rule with no criteria requires approval for every run, and that is what an operator relies on
// when they write the one blanket gate an install starts with. If any single field on a run could
// fall outside an empty rule, that gate has a hole in it that nothing on screen would show.
func TestAnEmptyRuleMatchesEveryRunShape(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 100000)
	tests := []struct {
		// Run is a run shape an empty rule must still cover.
		Run run.Run
	}{
		{Run: run.Run{}}, // Test 0: The zero run.
		{Run: run.Run{Tool: run.ToolBash, Command: "echo"}},      // Test 1: A shell run.
		{Run: run.Run{Tool: "", Command: ""}},                    // Test 2: The historical Ansible form.
		{Run: run.Run{Queue: "prod", InventoryID: "inv"}},        // Test 3: Routed and targeted.
		{Run: run.Run{ActorType: "webhook", Actor: "hook"}},      // Test 4: Fired by a webhook.
		{Run: run.Run{ActorType: "agent", Actor: "bot"}},         // Test 5: Fired by an agent.
		{Run: run.Run{DryRun: true}},                             // Test 6: A dry run, not excluded.
		{Run: run.Run{Command: long}},                            // Test 7: A very long command.
		{Run: run.Run{Command: "日本語 --force"}},                   // Test 8: Non-ASCII text.
		{Run: run.Run{Tool: "some-extension-tool"}},              // Test 9: A tool this build does not know.
		{Run: run.Run{Command: strings.Repeat("überall ", 500)}}, // Test 10: Long and non-ASCII.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := test.Run
			empty := policy.Policy{}
			if !empty.Matches(&r) {
				t.Errorf("an empty rule did not match run %+v, so the blanket gate has a hole in it", r)
			}
			if !policy.Requires([]*policy.Policy{blanket(empty)}, &r) {
				t.Errorf("an empty blanket rule did not require approval for run %+v", r)
			}
			deny := policy.Policy{Effect: policy.EffectDeny, MaxDestroy: policy.DisabledMaxDestroy}
			if policy.Denying([]*policy.Policy{&deny}, &r) == nil {
				t.Errorf("an empty deny rule did not refuse run %+v", r)
			}
		})
	}
}

// TestAdvancedIsTheSharedLicenseDefinition pins the one definition of what counts as the full policy
// engine.
//
// Every enforcement point is meant to share it so they cannot drift, and it decides whether a rule
// is refused under a Community license. A definition that answers false for a rule that is in fact
// advanced hands the paid engine away; one that answers true for a plain rule refuses the single
// gate a Community install is entitled to.
func TestAdvancedIsTheSharedLicenseDefinition(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Policy is the rule under test.
		Policy policy.Policy
		// WantAdvanced is whether the rule needs the full engine.
		WantAdvanced bool
	}{{ // Test 0: A plain require-approval rule is the Community gate.
		Policy: policy.Policy{MaxDestroy: policy.DisabledMaxDestroy}, WantAdvanced: false,
	}, { // Test 1: An explicit require_approval effect is still plain.
		Policy:       policy.Policy{Effect: policy.EffectRequireApproval, MaxDestroy: policy.DisabledMaxDestroy},
		WantAdvanced: false,
	}, { // Test 2: A deny effect is the full engine.
		Policy:       policy.Policy{Effect: policy.EffectDeny, MaxDestroy: policy.DisabledMaxDestroy},
		WantAdvanced: true,
	}, { // Test 3: A risk floor is the full engine.
		Policy:       policy.Policy{MinRisk: run.RiskLow, MaxDestroy: policy.DisabledMaxDestroy},
		WantAdvanced: true,
	}, { // Test 4: Separation of duties is the full engine.
		Policy:       policy.Policy{RequireDistinctApprover: true, MaxDestroy: policy.DisabledMaxDestroy},
		WantAdvanced: true,
	}, { // Test 5: Criteria that only narrow which runs a rule matches are not advanced on their
		// own, so a narrow Community rule stays allowed.
		Policy: policy.Policy{
			Tool: run.ToolTerraform, CommandContains: "destroy", InventoryID: "inv", Queue: "prod",
			ExcludeDryRun: true, MaxDestroy: policy.DisabledMaxDestroy,
		}, WantAdvanced: false,
	}, { // Test 6: A plan-content threshold alone is not the full engine either.
		Policy: policy.Policy{MaxDestroy: 5}, WantAdvanced: false,
	}, { // Test 7: Scoping by actor kind is the full engine. This case previously sat inside test 5,
		// bundled with the narrowing criteria and asserted not advanced, which pinned the gate's
		// behavior instead of the tier the product sells and so locked the defect in place.
		Policy: policy.Policy{
			ActorKind: policy.ActorKindAgent, MaxDestroy: policy.DisabledMaxDestroy,
		}, WantAdvanced: true,
	}, { // Test 8: Scoping to one named principal is the same criterion.
		Policy:       policy.Policy{Actor: "bot", MaxDestroy: policy.DisabledMaxDestroy},
		WantAdvanced: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			p := test.Policy
			if got := p.Advanced(); got != test.WantAdvanced {
				t.Errorf("Advanced() = %v, want %v for %+v", got, test.WantAdvanced, p)
			}
		})
	}
}

// TestNewPolicyDisablesThePlanContentGate pins what the constructor installs, which no rule-level
// test can catch.
//
// A policy whose MaxDestroy is zero is not a blanket rule at all: Requiring skips it, so it holds
// nothing at submission and instead waits for the plan gate. If the constructor ever handed back the
// zero value, every policy created through it would silently stop gating at submission while still
// reading as an approval rule everywhere it is displayed.
func TestNewPolicyDisablesThePlanContentGate(t *testing.T) {
	t.Parallel()
	p := policy.NewPolicy("prod gate")
	if p.MaxDestroy != policy.DisabledMaxDestroy {
		t.Fatalf("NewPolicy().MaxDestroy = %d, want %d: a new policy must be a blanket rule, not one "+
			"that only fires on plan content", p.MaxDestroy, policy.DisabledMaxDestroy)
	}
	if p.Name != "prod gate" {
		t.Errorf("Name = %q, want the name it was given", p.Name)
	}
	if !strings.HasPrefix(p.ID, "pol_") {
		t.Errorf("ID = %q, want a pol_ prefix", p.ID)
	}
	if p.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero, so the store cannot order policies oldest first")
	}
	if time.Since(p.CreatedAt) > time.Minute {
		t.Errorf("CreatedAt = %v, want roughly now", p.CreatedAt)
	}
	if err := p.Validate(); err != nil {
		t.Errorf("Validate() on a fresh policy = %v, want nil", err)
	}
	// The rule it installs is a blanket hold on everything, which is what a policy created with no
	// criteria means.
	if !policy.Requires([]*policy.Policy{p}, &run.Run{Tool: run.ToolBash, Command: "echo"}) {
		t.Error("a fresh policy did not hold a run at submission, so the gate it appears to be is not " +
			"the gate it is")
	}
	if policy.PlanGated([]*policy.Policy{p}, &run.Run{Tool: run.ToolTerraform}) {
		t.Error("a fresh policy scoped a run to the plan gate, so its hold would wait for a plan that " +
			"a non-terraform run never produces")
	}
	// Two policies never share an id, or an approval recorded against one resolves to the other.
	seen := map[string]bool{p.ID: true}
	for range 200 {
		id := policy.NewID()
		if seen[id] {
			t.Fatalf("NewID() repeated %q", id)
		}
		seen[id] = true
	}
}

// TestLabelNamesARuleInEvidence pins how a rule is named in the record that says why a run waited.
//
// The label is written onto the run and read by an auditor long after the rule may have been renamed
// or deleted, so a blank label is a hold nobody can explain.
func TestLabelNamesARuleInEvidence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Policy is the rule to label, nil for the nil case.
		Policy *policy.Policy
		// WantLabel is the label evidence must carry.
		WantLabel string
	}{{ // Test 0: A named rule is named.
		Policy: &policy.Policy{ID: "pol_1", Name: "prod gate"}, WantLabel: "prod gate",
	}, { // Test 1: An unnamed rule falls back to its id, so the record is never blank.
		Policy: &policy.Policy{ID: "pol_1"}, WantLabel: "pol_1",
	}, { // Test 2: A rule with neither has nothing to say, and must not panic saying it.
		Policy: &policy.Policy{}, WantLabel: "",
	}, { // Test 3: No rule at all is the empty label, which is what a run nothing held records.
		Policy: nil, WantLabel: "",
	}, { // Test 4: A name of only spaces is still a name, since trimming it would change what an
		// operator wrote.
		Policy: &policy.Policy{ID: "pol_1", Name: " "}, WantLabel: " ",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := test.Policy.Label(); got != test.WantLabel {
				t.Errorf("Label() = %q, want %q", got, test.WantLabel)
			}
		})
	}
}

// TestRequiringNamesTheFirstMatchingRule pins that the rule recorded on a held run is deterministic.
//
// The rule that held a run is evidence. If which rule gets named depended on anything but list
// order, two servers reading the same policies would write different reasons for the same hold, and
// an auditor comparing them would be looking at a difference nobody made.
func TestRequiringNamesTheFirstMatchingRule(t *testing.T) {
	t.Parallel()
	first := blanket(policy.Policy{ID: "pol_a", Name: "first", Tool: run.ToolTerraform})
	second := blanket(policy.Policy{ID: "pol_b", Name: "second"})
	r := &run.Run{Tool: run.ToolTerraform}

	if got := policy.Requiring([]*policy.Policy{first, second}, r); got != first {
		t.Errorf("Requiring() = %v, want the first matching rule", got)
	}
	if got := policy.Requiring([]*policy.Policy{second, first}, r); got != second {
		t.Errorf("Requiring() reordered = %v, want whichever rule is listed first", got)
	}
	if got := policy.Requiring(nil, r); got != nil {
		t.Errorf("Requiring(nil) = %v, want nil", got)
	}
	if policy.Requires(nil, r) {
		t.Error("an install with no policies held a run, so nothing could ever be dispatched")
	}
	// A plan-content rule is not an approval rule, so it never gets named as the one that held a run
	// at submission.
	planOnly := []*policy.Policy{{ID: "pol_c", Name: "plan", Tool: run.ToolTerraform, MaxDestroy: 0}}
	if got := policy.Requiring(planOnly, r); got != nil {
		t.Errorf("Requiring() = %v, want nil: a plan-content rule is enforced by the plan gate", got)
	}
	// A deny rule is not an approval rule either: a denied run is refused, never parked in front of
	// an approver who could release it.
	denyOnly := []*policy.Policy{blanket(policy.Policy{ID: "pol_d", Effect: policy.EffectDeny})}
	if got := policy.Requiring(denyOnly, r); got != nil {
		t.Errorf("Requiring() = %v, want nil for a deny rule", got)
	}
	if got := policy.Denying(nil, r); got != nil {
		t.Errorf("Denying(nil) = %v, want nil", got)
	}
}

// TestExceedingNamesTheViolatedRule pins the plan-content threshold at its boundary and pins which
// rule is recorded when a plan is held.
//
// The threshold is an off-by-one waiting to happen: a plan destroying exactly the permitted number
// must apply, and one resource more must hold. Getting it wrong in one direction applies a teardown
// nobody approved, and in the other holds every routine change forever.
func TestExceedingNamesTheViolatedRule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Policies are the rules in force.
		Policies []*policy.Policy
		// Destroys is how many resources the plan would destroy.
		Destroys int
		// WantID is the id of the rule the plan violates, empty when none does.
		WantID string
	}{{ // Test 0: One under the threshold applies.
		Policies: []*policy.Policy{{ID: "pol_1", Tool: run.ToolTerraform, MaxDestroy: 3}},
		Destroys: 2, WantID: "",
	}, { // Test 1: Exactly the threshold applies, since the rule permits that many.
		Policies: []*policy.Policy{{ID: "pol_1", Tool: run.ToolTerraform, MaxDestroy: 3}},
		Destroys: 3, WantID: "",
	}, { // Test 2: One over the threshold holds.
		Policies: []*policy.Policy{{ID: "pol_1", Tool: run.ToolTerraform, MaxDestroy: 3}},
		Destroys: 4, WantID: "pol_1",
	}, { // Test 3: A threshold of zero permits a plan that destroys nothing.
		Policies: []*policy.Policy{{ID: "pol_1", Tool: run.ToolTerraform, MaxDestroy: 0}},
		Destroys: 0, WantID: "",
	}, { // Test 4: A threshold of zero holds a plan that destroys one thing.
		Policies: []*policy.Policy{{ID: "pol_1", Tool: run.ToolTerraform, MaxDestroy: 0}},
		Destroys: 1, WantID: "pol_1",
	}, { // Test 5: A negative destroy count, which a broken plan parse could produce, holds nothing.
		Policies: []*policy.Policy{{ID: "pol_1", Tool: run.ToolTerraform, MaxDestroy: 0}},
		Destroys: -1, WantID: "",
	}, { // Test 6: The strictest matching rule is not sought; the first violated one is named, so the
		// record is deterministic.
		Policies: []*policy.Policy{
			{ID: "pol_loose", Tool: run.ToolTerraform, MaxDestroy: 10},
			{ID: "pol_tight", Tool: run.ToolTerraform, MaxDestroy: 1},
		}, Destroys: 5, WantID: "pol_tight",
	}, { // Test 7: A blanket rule never participates, whatever the plan destroys.
		Policies: []*policy.Policy{
			{ID: "pol_blanket", Tool: run.ToolTerraform, MaxDestroy: policy.DisabledMaxDestroy},
		}, Destroys: 9999, WantID: "",
	}, { // Test 8: A rule that does not cover this run is ignored.
		Policies: []*policy.Policy{{ID: "pol_1", Tool: run.ToolBash, MaxDestroy: 0}},
		Destroys: 100, WantID: "",
	}, { // Test 9: No rules at all hold nothing.
		Policies: nil, Destroys: 100, WantID: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := &run.Run{Tool: run.ToolTerraform, Command: "/infra"}
			got := policy.Exceeding(test.Policies, r, test.Destroys)
			gotID := ""
			if got != nil {
				gotID = got.ID
			}
			if gotID != test.WantID {
				t.Errorf("Exceeding() = %q, want %q", gotID, test.WantID)
			}
			if wantExceeds := test.WantID != ""; policy.PlanExceeds(test.Policies, r, test.Destroys) != wantExceeds {
				t.Errorf("PlanExceeds() disagrees with Exceeding() for %d destroys", test.Destroys)
			}
		})
	}
}

// TestValidateRefusesEveryTypo pins that the rule vocabulary is checked where the rule is written.
//
// A rule with a typo in it is the worst kind of gate: it is on screen, it reads as enforcement, and
// it matches nothing. Refusing it at the point of writing is the only place the person who made the
// mistake is still looking.
func TestValidateRefusesEveryTypo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Policy is the rule under test.
		Policy policy.Policy
		// WantErrContains is a fragment the refusal must name, empty when the rule is valid.
		WantErrContains string
	}{{ // Test 0: The empty rule is valid, and is the blanket gate.
		Policy: policy.Policy{MaxDestroy: policy.DisabledMaxDestroy}, WantErrContains: "",
	}, { // Test 1: An effect in the wrong case is not the effect.
		Policy:          policy.Policy{Effect: "DENY", MaxDestroy: policy.DisabledMaxDestroy},
		WantErrContains: "effect",
	}, { // Test 2: An effect with stray whitespace is refused rather than trimmed, because trimming
		// would accept a value no other code path recognizes.
		Policy:          policy.Policy{Effect: " deny", MaxDestroy: policy.DisabledMaxDestroy},
		WantErrContains: "effect",
	}, { // Test 3: An actor kind in the wrong case is refused.
		Policy:          policy.Policy{ActorKind: "Human", MaxDestroy: policy.DisabledMaxDestroy},
		WantErrContains: "actor_kind",
	}, { // Test 4: A risk level this build does not know is refused.
		Policy:          policy.Policy{MinRisk: "critical", MaxDestroy: policy.DisabledMaxDestroy},
		WantErrContains: "min_risk",
	}, { // Test 5: A risk level in the wrong case is refused.
		Policy:          policy.Policy{MinRisk: "HIGH", MaxDestroy: policy.DisabledMaxDestroy},
		WantErrContains: "min_risk",
	}, { // Test 6: Deny with a threshold of zero is a contradiction: a denied run is never planned.
		Policy:          policy.Policy{Effect: policy.EffectDeny, MaxDestroy: 0},
		WantErrContains: "max_destroy",
	}, { // Test 7: Deny with a large threshold is the same contradiction.
		Policy:          policy.Policy{Effect: policy.EffectDeny, MaxDestroy: 50},
		WantErrContains: "max_destroy",
	}, { // Test 8: Deny with the threshold disabled is the ordinary deny rule.
		Policy:          policy.Policy{Effect: policy.EffectDeny, MaxDestroy: policy.DisabledMaxDestroy},
		WantErrContains: "",
	}, { // Test 9: A threshold below the disabled value is still disabled and still valid, so an older
		// row does not become unloadable.
		Policy: policy.Policy{MaxDestroy: -50}, WantErrContains: "",
	}, { // Test 10: Every valid word together.
		Policy: policy.Policy{
			Effect: policy.EffectRequireApproval, ActorKind: policy.ActorKindHuman,
			MinRisk: run.RiskMedium, MaxDestroy: 7, RequireDistinctApprover: true,
		}, WantErrContains: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			p := test.Policy
			err := p.Validate()
			if test.WantErrContains == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() accepted %+v, so the typo becomes a rule that gates nothing", p)
			}
			if !strings.Contains(err.Error(), test.WantErrContains) {
				t.Errorf("Validate() = %q, want it to name %q so the writer knows which field is "+
					"wrong", err, test.WantErrContains)
			}
		})
	}
}

// TestRequireDistinctIgnoresRulesThatDoNotApply pins the edges of separation of duties.
//
// The requirement composes by OR across every matching rule, so the failure to avoid is one where a
// rule that does not cover this run adds a second approver anyway, or where a deny rule's flag
// leaks into a hold it has nothing to do with.
func TestRequireDistinctIgnoresRulesThatDoNotApply(t *testing.T) {
	t.Parallel()
	r := &run.Run{Tool: run.ToolTerraform, Queue: "prod", ActorType: "session"}
	tests := []struct {
		// Policies are the rules in force.
		Policies []*policy.Policy
		// WantDistinct is whether a second person must decide.
		WantDistinct bool
	}{{ // Test 0: No rules demand nothing.
		Policies: nil, WantDistinct: false,
	}, { // Test 1: A matching rule with the flag demands a second person.
		Policies:     []*policy.Policy{blanket(policy.Policy{Queue: "prod", RequireDistinctApprover: true})},
		WantDistinct: true,
	}, { // Test 2: A deny rule's flag stays out of it, because a denied run is never approved at all.
		Policies: []*policy.Policy{blanket(policy.Policy{
			Effect: policy.EffectDeny, RequireDistinctApprover: true,
		})}, WantDistinct: false,
	}, { // Test 3: A rule scoped to another queue does not reach this run.
		Policies: []*policy.Policy{blanket(policy.Policy{
			Queue: "staging", RequireDistinctApprover: true,
		})}, WantDistinct: false,
	}, { // Test 4: A rule scoped to agents does not reach a person's run.
		Policies: []*policy.Policy{blanket(policy.Policy{
			ActorKind: policy.ActorKindAgent, RequireDistinctApprover: true,
		})}, WantDistinct: false,
	}, { // Test 5: A plan-content rule demanding a second person counts, since the plan gate holds
		// under the same requirement.
		Policies:     []*policy.Policy{{Tool: run.ToolTerraform, MaxDestroy: 0, RequireDistinctApprover: true}},
		WantDistinct: true,
	}, { // Test 6: One matching rule out of many is enough.
		Policies: []*policy.Policy{
			blanket(policy.Policy{Queue: "staging", RequireDistinctApprover: true}),
			blanket(policy.Policy{Tool: run.ToolBash, RequireDistinctApprover: true}),
			blanket(policy.Policy{Tool: run.ToolTerraform, RequireDistinctApprover: true}),
		}, WantDistinct: true,
	}, { // Test 7: Matching rules without the flag demand nothing.
		Policies: []*policy.Policy{
			blanket(policy.Policy{Queue: "prod"}), blanket(policy.Policy{Tool: run.ToolTerraform}),
		}, WantDistinct: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := policy.RequireDistinct(test.Policies, r); got != test.WantDistinct {
				t.Errorf("RequireDistinct() = %v, want %v", got, test.WantDistinct)
			}
		})
	}
}

// TestPolicyJSONRoundTrip pins that a rule survives the encoding it is stored and served in.
//
// A field lost in transit is a rule that quietly does something else on the other side, and
// max_destroy is the field where that is worst: dropped, it decodes as zero, which turns a blanket
// gate into a plan-content rule that holds nothing at submission.
func TestPolicyJSONRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Policy is the rule to encode and decode.
		Policy policy.Policy
	}{{ // Test 0: Every field set.
		Policy: policy.Policy{
			ID: "pol_1", Name: "prod destroy", Tool: run.ToolTerraform, CommandContains: "destroy",
			InventoryID: "inv_prod", Queue: "prod", ActorKind: policy.ActorKindAgent, Actor: "bot",
			MinRisk: run.RiskHigh, Effect: policy.EffectDeny, ExcludeDryRun: true,
			RequireDistinctApprover: true, MaxDestroy: policy.DisabledMaxDestroy,
			CreatedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		},
	}, { // Test 1: A blanket rule with nothing but a name, which is the common shape.
		Policy: policy.Policy{
			ID: "pol_2", Name: "hold everything", MaxDestroy: policy.DisabledMaxDestroy,
			CreatedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		},
	}, { // Test 2: A plan-content rule with a threshold of zero, the value most easily lost.
		Policy: policy.Policy{
			ID: "pol_3", Name: "any destroy", Tool: run.ToolOpenTofu, MaxDestroy: 0,
			CreatedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(test.Policy)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			var got policy.Policy
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if diff := cmp.Diff(test.Policy, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("round trip mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
