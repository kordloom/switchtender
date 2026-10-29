package policy_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/policytest"
	"github.com/kordloom/switchtender/internal/run"
)

// scopeNotApplied is what a Community hold adds to its label when the rule it stands for is scoped
// to an actor, an actor kind, or an account.
const scopeNotApplied = "; its actor scope is not applied on this build)"

// rule returns a blanket rule named name with the given criteria set on it.
func rule(name string, set func(p *policy.Policy)) *policy.Policy {
	p := &policy.Policy{ID: "pol_" + name, Name: name, MaxDestroy: policy.DisabledMaxDestroy}
	set(p)
	return p
}

// humanRun returns a run a signed-in person submitted on account.
func humanRun(tool, command, account string) *run.Run {
	return &run.Run{Actor: "dev-lead", ActorType: "session", ActorUserID: "usr_" + account,
		Account: account, Tool: tool, Command: command}
}

// agentOn returns a run the named agent token submitted on account.
func agentOn(name, account string) *run.Run {
	return &run.Run{Actor: name, ActorType: policy.ActorKindAgent, ActorUserID: "usr_" + account,
		Account: account, Tool: run.ToolBash, Command: "./deploy.sh"}
}

// TestTheCommunityEngineIsTheOneInForce pins that a build without the paid module evaluates every
// rule through the fail-closed stub, which is what every other test in this file relies on.
func TestTheCommunityEngineIsTheOneInForce(t *testing.T) {
	t.Parallel()
	if policy.EngineRegistered() {
		t.Fatal("a rule engine is registered in a build without the paid module")
	}
}

// TestCommunityHoldsWhatItCannotEvaluate pins the Community answer to every advanced rule: it holds
// the runs the rule's free criteria cover, names the rule and the tier on the hold, refuses
// nothing, and leaves alone the runs those criteria do not cover. A rule scoped to an actor, a kind,
// or an account cannot be scoped on this build, so it holds every run its other criteria cover and
// the label says so, which holds more than the rule asked and never less.
func TestCommunityHoldsWhatItCannotEvaluate(t *testing.T) {
	t.Parallel()
	pro := license.Unlicensed(license.FeaturePolicyRules).Error()
	team := license.Unlicensed(license.FeaturePolicyFull).Error()
	tests := []struct {
		// Rule is the one rule in force.
		Rule *policy.Policy
		// Run is the run it is asked about.
		Run *run.Run
		// WantHeldBy is the label of the hold, empty when nothing holds the run.
		WantHeldBy string
	}{{ // Test 0: A deny rule holds the run it covers, naming Pro.
		Rule: rule("no drops", func(p *policy.Policy) {
			p.Tool, p.CommandContains, p.Effect = run.ToolBash, "drop database", policy.EffectDeny
		}),
		Run:        humanRun(run.ToolBash, "psql -c 'DROP DATABASE prod'", "dba"),
		WantHeldBy: policytest.CommunityHold("no drops", license.FeaturePolicyRules),
	}, { // Test 1: A deny rule's free criteria are still read, so a run it does not cover goes ahead.
		Rule: rule("no drops", func(p *policy.Policy) {
			p.Tool, p.CommandContains, p.Effect = run.ToolBash, "drop database", policy.EffectDeny
		}),
		Run: humanRun(run.ToolBash, "uptime", "dba"),
	}, { // Test 2: A floor this build cannot read applies, so a reversible run is held too.
		Rule: rule("hold the permanent", func(p *policy.Policy) {
			p.Tool, p.Reversibility = run.ToolAnsible, run.Irreversible
		}),
		Run:        humanRun(run.ToolAnsible, "restart.yml", "ops"),
		WantHeldBy: policytest.CommunityHold("hold the permanent", license.FeaturePolicyRules),
	}, { // Test 3: An agent-scoped rule holds a person's run its tool criterion covers.
		Rule: rule("agents on terraform", func(p *policy.Policy) {
			p.Tool, p.ActorKind = run.ToolTerraform, policy.ActorKindAgent
		}),
		Run: humanRun(run.ToolTerraform, "infra", "ops"),
		WantHeldBy: fmt.Sprintf("agents on terraform (held because this build cannot evaluate it: "+
			"%s%s", team, scopeNotApplied),
	}, { // Test 4: The same rule leaves a person's Ansible run alone, so the widening is not a
		// blanket hold.
		Rule: rule("agents on terraform", func(p *policy.Policy) {
			p.Tool, p.ActorKind = run.ToolTerraform, policy.ActorKindAgent
		}),
		Run: humanRun(run.ToolAnsible, "site.yml", "ops"),
	}, { // Test 5: An account-scoped rule holds a run on another account.
		Rule: rule("svc changes", func(p *policy.Policy) {
			p.Tool, p.Account = run.ToolBash, "svc"
		}),
		Run: humanRun(run.ToolBash, "uptime", "other"),
		WantHeldBy: fmt.Sprintf("svc changes (held because this build cannot evaluate it: %s%s",
			team, scopeNotApplied),
	}, { // Test 6: A plain rule is the Community gate and holds under its own name.
		Rule:       rule("bash needs a person", func(p *policy.Policy) { p.Tool = run.ToolBash }),
		Run:        humanRun(run.ToolBash, "uptime", "ops"),
		WantHeldBy: "bash needs a person",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			set := []*policy.Policy{test.Rule}
			if got := policy.Denying(set, test.Run); got != nil {
				t.Errorf("Denying() = %q, want nil: a build without the rule engine refuses nothing "+
					"on a rule it cannot evaluate", got.Label())
			}
			got := policy.Requiring(set, test.Run)
			if test.WantHeldBy == "" {
				if got != nil {
					t.Errorf("Requiring() = %q, want nil", got.Label())
				}
				return
			}
			if got == nil {
				t.Fatalf("Requiring() = nil, want held by %q", test.WantHeldBy)
			}
			if got.Label() != test.WantHeldBy {
				t.Errorf("held by %q, want %q", got.Label(), test.WantHeldBy)
			}
			if got.Effect != policy.EffectRequireApproval && got.Effect != "" {
				t.Errorf("hold effect = %q, want an approval hold", got.Effect)
			}
			if test.Rule.Advanced() && !strings.Contains(got.Label(), pro) &&
				!strings.Contains(got.Label(), team) {
				t.Errorf("held by %q names no tier", got.Label())
			}
		})
	}
}

// TestCommunityEnforcesARequirementItCannotNarrow pins separation of duties and the reason
// requirement on Community. A rule carrying them that covers the run on its free criteria demands
// them, whatever account or actor it was scoped to, since this build cannot apply the scope that
// would have narrowed it. A rule whose free criteria do not cover the run demands nothing.
func TestCommunityEnforcesARequirementItCannotNarrow(t *testing.T) {
	t.Parallel()
	scoped := rule("svc two people", func(p *policy.Policy) {
		p.Account, p.RequireDistinctApprover, p.RequireReason = "svc", true, decision.RequireAlways
	})
	terraformOnly := rule("terraform two people", func(p *policy.Policy) {
		p.Tool, p.RequireDistinctApprover, p.RequireReason = run.ToolTerraform, true,
			decision.RequireAlways
	})
	other := humanRun(run.ToolBash, "uptime", "other")
	if !policy.RequireDistinct([]*policy.Policy{scoped}, other) {
		t.Error("an account-scoped rule demanding a second person was dropped for a run on " +
			"another account, so a scope this build cannot apply narrowed the rule")
	}
	if got := policy.ReasonRequirement([]*policy.Policy{scoped}, other); got != decision.RequireAlways {
		t.Errorf("ReasonRequirement() = %q, want %q", got, decision.RequireAlways)
	}
	if policy.RequireDistinct([]*policy.Policy{terraformOnly}, other) {
		t.Error("a terraform rule demanded a second person for a bash run, the control")
	}
	if got := policy.ReasonRequirement([]*policy.Policy{terraformOnly}, other); got != "" {
		t.Errorf("ReasonRequirement() = %q for a run the rule does not cover, want none", got)
	}
}

// TestCommunityPlansAFloorRuleFirst pins that a floor rule covering an unplanned apply sends it to
// the plan gate, and that the apply its plan proposes is held naming the tier.
func TestCommunityPlansAFloorRuleFirst(t *testing.T) {
	t.Parallel()
	floor := rule("irreversible applies", func(p *policy.Policy) {
		p.Tool, p.Reversibility = run.ToolTerraform, run.Irreversible
	})
	set := []*policy.Policy{floor}
	apply := humanRun(run.ToolTerraform, "infra", "ops")
	apply.Status = run.StatusPending
	if !policy.PlanGated(set, apply) {
		t.Error("a floor rule this build cannot read did not plan the apply first")
	}
	proposal := humanRun(run.ToolTerraform, "infra", "ops")
	proposal.ProposedFrom = "run_plan"
	got := policy.Requiring(set, proposal)
	want := policytest.CommunityHold("irreversible applies", license.FeaturePolicyRules)
	if got == nil || got.Label() != want {
		t.Errorf("proposal held by %q, want %q", got.Label(), want)
	}
	if policy.PlanGated(set, humanRun(run.ToolAnsible, "site.yml", "ops")) {
		t.Error("an Ansible run was plan gated, and only an unplanned apply ever is")
	}
}

// TestCommunityWeighsAnAdvancedDestroyLimit pins a plan-content rule that is also advanced: a plan
// over its limit is held naming the tier and demands the rule's second person, whatever actor it was
// scoped to, and a plan within the limit is not held.
func TestCommunityWeighsAnAdvancedDestroyLimit(t *testing.T) {
	t.Parallel()
	limit := &policy.Policy{ID: "pol_limit", Name: "agent destroys", Tool: run.ToolTerraform,
		ActorKind: policy.ActorKindAgent, MaxDestroy: 0, RequireDistinctApprover: true}
	set := []*policy.Policy{limit}
	r := humanRun(run.ToolTerraform, "infra", "ops")
	got := policy.Exceeding(set, r, 1)
	team := license.Unlicensed(license.FeaturePolicyFull).Error()
	want := fmt.Sprintf("agent destroys (held because this build cannot evaluate it: %s%s", team,
		scopeNotApplied)
	if got == nil || got.Label() != want {
		t.Errorf("Exceeding() = %q, want %q", got.Label(), want)
	}
	if !policy.ExceedingDistinct(set, r, 1) {
		t.Error("the exceeded rule's second person was dropped")
	}
	if got := policy.Exceeding(set, r, 0); got != nil {
		t.Errorf("Exceeding() = %q for a plan within the limit, want nil", got.Label())
	}
}

// TestCommunityNeverWidensAnExemption pins that an exemption keeps every criterion it has, scope
// included. Widening a hold holds more; widening an exemption would let more through.
func TestCommunityNeverWidensAnExemption(t *testing.T) {
	t.Parallel()
	exempt := &policy.Policy{ID: "pol_ci", Name: "ci deploys", Effect: policy.EffectExempt,
		Actor: "ci", Account: "svc", MaxDestroy: policy.DisabledMaxDestroy}
	set := []*policy.Policy{exempt}
	if got := policy.Requiring(set, agentOn("ci", "svc")); got != nil {
		t.Errorf("the exempted agent's run was held by %q", got.Label())
	}
	got := policy.Requiring(set, agentOn("ci", "other"))
	if got == nil || got.Label() != policy.AgentDefault().Label() {
		t.Errorf("the agent on another account was held by %q, want the built-in agent hold: the "+
			"exemption was widened past its account", got.Label())
	}
}

// TestCommunityRegoDecisions pins the two Rego answers a Community build gives. A stored program
// holds every run, since this build cannot evaluate it. A program that was never compiled refuses,
// labeled rather than crashing the gate on its missing digest.
func TestCommunityRegoDecisions(t *testing.T) {
	t.Parallel()
	r := humanRun(run.ToolBash, "uptime", "ops")
	stored := []*policy.Policy{{ID: "pol_r", Name: "rego gate", MaxDestroy: policy.DisabledMaxDestroy,
		Rego: policytest.StoredRego(t)}}
	if got := policy.Requiring(stored, r); got == nil ||
		!strings.Contains(got.Label(), policy.RegoUnavailableReason) {
		t.Errorf("a stored Rego policy held by %q, want a hold naming why it was not evaluated",
			got.Label())
	}
	broken := []*policy.Policy{policytest.Refusing("broken")}
	got := policy.Denying(broken, r)
	if got == nil || !strings.Contains(got.Label(), "never compiled") {
		t.Errorf("an uncompiled program refused as %q, want a refusal saying it was never compiled",
			got.Label())
	}
}
