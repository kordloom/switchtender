package policy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/run"
)

// Findings the agent hold tests put on dry runs, in the words the gate's scans record them.
const (
	// forcedFinding is an Ansible task that runs for real under --check.
	forcedFinding = `site.yml: task "Restart web" sets check_mode to false`
	// externalFinding is a Terraform external data source, which runs a program while it plans.
	externalFinding = "data.external.x runs a program during plan (main.tf line 1)"
)

// agentRun returns a run the named agent token submitted.
func agentRun(name, tool, command string) *run.Run {
	return &run.Run{Actor: name, ActorType: ActorKindAgent, Tool: tool, Command: command}
}

// personRun returns a run a signed-in person submitted.
func personRun(tool, command string) *run.Run {
	return &run.Run{Actor: "dev-lead", ActorType: "session", Tool: tool, Command: command}
}

// boundAgentRun returns a run the named agent token submitted, bound to the named account.
func boundAgentRun(name, account, tool, command string) *run.Run {
	r := agentRun(name, tool, command)
	r.Account = account
	return r
}

// triageBot scopes an exemption to the triage agent on the ops account.
func triageBot(p *Policy) { p.Actor, p.Account = "triage-bot", "ops" }

// exemption returns an exemption with the given criteria set on it.
func exemption(name string, set func(p *Policy)) *Policy {
	p := &Policy{ID: "pol_" + name, Name: name, Effect: EffectExempt, MaxDestroy: DisabledMaxDestroy}
	if set != nil {
		set(p)
	}
	return p
}

// TestAgentRequested holds who counts as an agent to the minted token kind and the identity a
// derived run carries, never to how a request looked.
func TestAgentRequested(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Run is the run asked about.
		Run *run.Run
		// WantAgent is whether an agent requested it.
		WantAgent bool
	}{{ // Test 0: A run an agent token submitted.
		Run: &run.Run{ActorType: ActorKindAgent}, WantAgent: true,
	}, { // Test 1: A run derived from an agent's request carries the agent's identity.
		Run:       &run.Run{ActorType: "system", Initiator: &run.Initiator{InitiatedBy: "bot"}},
		WantAgent: true,
	}, { // Test 2: A signed-in person.
		Run: &run.Run{ActorType: "session"}, WantAgent: false,
	}, { // Test 3: An owner-held API token.
		Run: &run.Run{ActorType: "token"}, WantAgent: false,
	}, { // Test 4: A trigger's webhook.
		Run: &run.Run{ActorType: "webhook"}, WantAgent: false,
	}, { // Test 5: No run at all.
		Run: nil, WantAgent: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := AgentRequested(test.Run); got != test.WantAgent {
				t.Errorf("AgentRequested() = %v, want %v", got, test.WantAgent)
			}
		})
	}
}

// TestTheBuiltInAgentHold walks the built-in hold through Requiring and AgentNote, the two answers
// the dispatcher records: which rule held a run, and what the evidence says about the hold. Each
// case that holds sits beside the control that does not, so the hold is shown to come from who
// asked, from what a dry run was found to do, or from a missing exemption, and from nothing else.
//
//nolint:funlen // Test function.
func TestTheBuiltInAgentHold(t *testing.T) {
	t.Parallel()
	holdBash := &Policy{ID: "pol_bash", Name: "hold bash", Tool: "bash",
		MaxDestroy: DisabledMaxDestroy}
	smoke := exemption("nightly smoke", func(p *Policy) {
		p.Tool, p.CommandContains = "bash", "smoke"
	})
	exemptNote := `requested by an agent, exempt from the default hold by policy "nightly smoke"`
	dryRun := func(r *run.Run, scans ...run.DryRunScan) *run.Run {
		r.DryRun, r.DryRunScans = true, scans
		return r
	}
	tests := []struct {
		// Policies are the stored rules in force.
		Policies []*Policy
		// Run is the run judged.
		Run *run.Run
		// WantHeldBy names the rule that holds the run, empty when nothing does.
		WantHeldBy string
		// WantNote is what the evidence records about the built-in hold.
		WantNote string
	}{{ // Test 0: An agent's run waits for a person with no policy written at all.
		Run:        agentRun("bot", "bash", "systemctl restart web"),
		WantHeldBy: AgentDefaultName, WantNote: AgentDefaultName,
	}, { // Test 1: The same run from a person is not held, the control for test 0.
		Run: personRun("bash", "systemctl restart web"),
	}, { // Test 2: A run carrying an agent's identity is held as the agent's.
		Run: &run.Run{Actor: "system:scheduler", ActorType: "system", Tool: "bash",
			Command: "deploy", Initiator: &run.Initiator{InitiatedBy: "bot", BoundTo: "dev-lead"}},
		WantHeldBy: AgentDefaultName, WantNote: AgentDefaultName,
	}, { // Test 3: An agent's dry run shown to change nothing proceeds and records nothing.
		Run: dryRun(agentRun("bot", "ansible", ""), run.DryRunScan{Tool: "ansible",
			Inputs: []string{"site.yml"}}),
	}, { // Test 4: An agent's dry run whose playbook forces real work is held.
		Run: dryRun(agentRun("bot", "ansible", ""), run.DryRunScan{Tool: "ansible",
			Findings: []string{forcedFinding}}),
		WantHeldBy: AgentDefaultName, WantNote: AgentDefaultName,
	}, { // Test 5: An agent's plan whose configuration runs a program is held.
		Run: dryRun(agentRun("bot", "terraform", "infra"), run.DryRunScan{Tool: "terraform",
			Findings: []string{externalFinding}}),
		WantHeldBy: AgentDefaultName, WantNote: AgentDefaultName,
	}, { // Test 6: An agent's plan the gate could not read in full is held.
		Run: dryRun(agentRun("bot", "opentofu", "infra"), run.DryRunScan{Tool: "opentofu",
			Unread: []string{`module.vpc from "acme/vpc/aws" (not downloaded here)`}}),
		WantHeldBy: AgentDefaultName, WantNote: AgentDefaultName,
	}, { // Test 7: A person's forcing dry run is not held by the built-in, the control for test 4.
		Run: dryRun(personRun("ansible", ""), run.DryRunScan{Tool: "ansible",
			Findings: []string{forcedFinding}}),
	}, { // Test 8: A stored rule that holds the run is named before the built-in.
		Policies: []*Policy{holdBash}, Run: agentRun("bot", "bash", "deploy"),
		WantHeldBy: "hold bash", WantNote: AgentDefaultName,
	}, { // Test 9: An exemption lets the agent's run proceed and the evidence names it.
		Policies: []*Policy{smoke}, Run: agentRun("bot", "bash", "run smoke tests"),
		WantNote: exemptNote,
	}, { // Test 10: An exemption lifts only the built-in hold: a stored rule still holds.
		Policies: []*Policy{smoke, holdBash}, Run: agentRun("bot", "bash", "run smoke tests"),
		WantHeldBy: "hold bash", WantNote: exemptNote,
	}, { // Test 11: The command criterion ignores case.
		Policies: []*Policy{smoke}, Run: agentRun("bot", "bash", "RUN SMOKE TESTS"),
		WantNote: exemptNote,
	}, { // Test 12: A command the exemption does not name is still held.
		Policies: []*Policy{smoke}, Run: agentRun("bot", "bash", "rm -rf /var/data"),
		WantHeldBy: AgentDefaultName, WantNote: AgentDefaultName,
	}, { // Test 13: A tool the exemption does not name is still held.
		Policies: []*Policy{smoke}, Run: agentRun("bot", "python", "smoke.py"),
		WantHeldBy: AgentDefaultName, WantNote: AgentDefaultName,
	}, { // Test 14: An exemption by queue covers the queue it names.
		Policies: []*Policy{exemption("staging queue", func(p *Policy) { p.Queue = "staging" })},
		Run: func() *run.Run {
			r := agentRun("bot", "bash", "deploy")
			r.Queue = "staging"
			return r
		}(),
		WantNote: `requested by an agent, exempt from the default hold by policy "staging queue"`,
	}, { // Test 15: And no other queue.
		Policies: []*Policy{exemption("staging queue", func(p *Policy) { p.Queue = "staging" })},
		Run: func() *run.Run {
			r := agentRun("bot", "bash", "deploy")
			r.Queue = "production"
			return r
		}(),
		WantHeldBy: AgentDefaultName, WantNote: AgentDefaultName,
	}, { // Test 16: An exemption by inventory covers a run targeting it.
		Policies: []*Policy{exemption("lab hosts", func(p *Policy) { p.InventoryID = "inv_lab" })},
		Run: func() *run.Run {
			r := agentRun("bot", "ansible", "")
			r.InventoryID = "inv_lab"
			return r
		}(),
		WantNote: `requested by an agent, exempt from the default hold by policy "lab hosts"`,
	}, { // Test 17: And no other inventory.
		Policies: []*Policy{exemption("lab hosts", func(p *Policy) { p.InventoryID = "inv_lab" })},
		Run: func() *run.Run {
			r := agentRun("bot", "ansible", "")
			r.InventoryID = "inv_prod"
			return r
		}(),
		WantHeldBy: AgentDefaultName, WantNote: AgentDefaultName,
	}, { // Test 18: An exemption naming one agent and its account covers that agent.
		Policies: []*Policy{exemption("triage bot", triageBot)},
		Run:      boundAgentRun("triage-bot", "ops", "bash", "collect logs"),
		WantNote: `requested by an agent bound to account "ops", exempt from the default hold ` +
			`by policy "triage bot"`,
	}, { // Test 19: And no other agent.
		Policies:   []*Policy{exemption("triage bot", triageBot)},
		Run:        boundAgentRun("release-bot", "ops", "bash", "collect logs"),
		WantHeldBy: AgentDefaultName, WantNote: AgentDefaultName,
	}, { // Test 20: An exemption scoped to agents covers a run carrying an agent's identity.
		Policies: []*Policy{exemption("agents", func(p *Policy) { p.ActorKind = ActorKindAgent })},
		Run: &run.Run{Actor: "system:scheduler", ActorType: "system", Tool: "bash",
			Command: "deploy", Initiator: &run.Initiator{InitiatedBy: "bot"}},
		WantNote: `requested by an agent, exempt from the default hold by policy "agents"`,
	}, { // Test 21: An exemption never holds a person's run it matches.
		Policies: []*Policy{smoke}, Run: personRun("bash", "run smoke tests"),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			gotHeldBy := ""
			if p := Requiring(test.Policies, test.Run); p != nil {
				gotHeldBy = p.Label()
			}
			if diff := cmp.Diff(test.WantHeldBy, gotHeldBy); diff != "" {
				t.Errorf("held by mismatch (-want +got):\n%s", diff)
			}
			if got := AgentHolds(test.Policies, test.Run); got != (test.WantNote == AgentDefaultName) {
				t.Errorf("AgentHolds() = %v, want %v", got, test.WantNote == AgentDefaultName)
			}
			if diff := cmp.Diff(test.WantNote, AgentNote(test.Policies, test.Run)); diff != "" {
				t.Errorf("AgentNote() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestTheBuiltInHoldIsMarked holds the rule Requiring returns for the built-in hold to being
// recognizable as the built-in, and a stored rule that happens to share its name to not being.
func TestTheBuiltInHoldIsMarked(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Policy is the rule asked about.
		Policy *Policy
		// WantBuiltIn is whether it is the built-in hold.
		WantBuiltIn bool
	}{{ // Test 0: The rule Requiring returns for an unexempted agent run.
		Policy: Requiring(nil, agentRun("bot", "bash", "deploy")), WantBuiltIn: true,
	}, { // Test 1: A stored rule with the same name is still a stored rule.
		Policy:      &Policy{ID: "pol_lookalike", Name: AgentDefaultName, MaxDestroy: -1},
		WantBuiltIn: false,
	}, { // Test 2: No rule at all.
		Policy: nil, WantBuiltIn: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := IsAgentDefault(test.Policy); got != test.WantBuiltIn {
				t.Errorf("IsAgentDefault() = %v, want %v", got, test.WantBuiltIn)
			}
		})
	}
	if p := AgentDefault(); p.Advanced() || p.Denies() || p.Exempts() || p.MaxDestroy >= 0 ||
		p.RequireDistinctApprover || p.RequireReason != "" {
		t.Errorf("AgentDefault() = %+v, want a plain hold that adds no requirement", p)
	}
}

// TestAnExemptionNeverOutranksADeny holds an exemption to lifting the built-in hold and nothing
// else: a deny rule matching the same agent run still refuses it.
func TestAnExemptionNeverOutranksADeny(t *testing.T) {
	t.Parallel()
	deny := &Policy{ID: "pol_deny", Name: "no drops", CommandContains: "drop database",
		Effect: EffectDeny, MaxDestroy: DisabledMaxDestroy}
	exempt := exemption("all bash", func(p *Policy) { p.Tool = "bash" })
	tests := []struct {
		// Policies are the rules in force, in order.
		Policies []*Policy
		// Run is the run judged.
		Run *run.Run
		// WantDenied names the rule that refuses the run, empty when none does.
		WantDenied string
	}{{ // Test 0: The deny refuses the agent's run though an exemption also covers it.
		Policies: []*Policy{exempt, deny}, Run: agentRun("bot", "bash", "psql -c 'drop database x'"),
		WantDenied: "no drops",
	}, { // Test 1: Listed first or last, the order changes nothing.
		Policies: []*Policy{deny, exempt}, Run: agentRun("bot", "bash", "psql -c 'drop database x'"),
		WantDenied: "no drops",
	}, { // Test 2: An exemption alone refuses nothing, the control for tests 0 and 1.
		Policies: []*Policy{exempt}, Run: agentRun("bot", "bash", "psql -c 'drop database x'"),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := ""
			if p := Denying(test.Policies, test.Run); p != nil {
				got = p.Label()
			}
			if diff := cmp.Diff(test.WantDenied, got); diff != "" {
				t.Errorf("denied by mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestValidateAnExemption holds an exemption to the criteria that pick runs out. Everything that
// only means something on a rule that holds is refused where it is written, since an exemption
// carrying it would read as narrower or stricter than it is.
func TestValidateAnExemption(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Set changes the exemption under test.
		Set func(p *Policy)
		// WantRefused is whether Validate refuses it.
		WantRefused bool
	}{{ // Test 0: No criteria at all exempts every agent run, which is allowed and documented.
		Set: nil, WantRefused: false,
	}, { // Test 1: Every criterion an exemption takes.
		Set: func(p *Policy) {
			p.Tool, p.CommandContains, p.InventoryID, p.Queue = "bash", "smoke", "inv_lab", "staging"
			p.Actor, p.Account, p.ActorKind = "triage-bot", "ops", ActorKindAgent
		},
		WantRefused: false,
	}, { // Test 2: Scoped to people, it would lift a hold people never have.
		Set: func(p *Policy) { p.ActorKind = ActorKindHuman }, WantRefused: true,
	}, { // Test 3: A risk floor would exempt the riskiest runs and hold the routine ones.
		Set: func(p *Policy) { p.MinRisk = run.RiskHigh }, WantRefused: true,
	}, { // Test 4: A reversibility floor would do the same.
		Set: func(p *Policy) { p.Reversibility = run.Irreversible }, WantRefused: true,
	}, { // Test 5: Excluding dry runs means nothing on a rule that holds nothing.
		Set: func(p *Policy) { p.ExcludeDryRun = true }, WantRefused: true,
	}, { // Test 6: An approver requirement has no decision to apply to.
		Set: func(p *Policy) { p.RequireDistinctApprover = true }, WantRefused: true,
	}, { // Test 7: Neither has a reason requirement.
		Set: func(p *Policy) { p.RequireReason = "always" }, WantRefused: true,
	}, { // Test 8: A destroy threshold belongs on a plan-content rule.
		Set: func(p *Policy) { p.MaxDestroy = 3 }, WantRefused: true,
	}, { // Test 9: The same threshold on a rule that holds is accepted, the control for test 8.
		Set:         func(p *Policy) { p.Effect, p.Tool, p.MaxDestroy = "", "terraform", 3 },
		WantRefused: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := exemption("under test", test.Set).Validate()
			if refused := err != nil; refused != test.WantRefused {
				t.Errorf("Validate() = %v, want refused %v", err, test.WantRefused)
			}
		})
	}
}

// TestAnExemptionIsCommunity holds writing an exemption to the free tier. The built-in hold is the
// default every install gets, so the rule that lets an agent's routine work past it cannot be one
// the install has to buy, even when it names the agent it covers.
func TestAnExemptionIsCommunity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Policy is the rule asked about.
		Policy Policy
		// WantFull is whether it is the full policy engine.
		WantFull bool
	}{{ // Test 0: An exemption naming one agent.
		Policy: Policy{Name: "triage", Effect: EffectExempt, Actor: "triage-bot"}, WantFull: false,
	}, { // Test 1: An exemption scoped to agents as a class.
		Policy:   Policy{Name: "agents", Effect: EffectExempt, ActorKind: ActorKindAgent},
		WantFull: false,
	}, { // Test 2: The same named agent on a rule that holds is the full engine, the control.
		Policy: Policy{Name: "triage", Actor: "triage-bot"}, WantFull: true,
	}, { // Test 3: The same class on a rule that holds is too.
		Policy: Policy{Name: "agents", ActorKind: ActorKindAgent}, WantFull: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := test.Policy.Advanced(); got != test.WantFull {
				t.Errorf("Advanced() = %v, want %v", got, test.WantFull)
			}
		})
	}
}

// TestInForceDescribesAnExemption holds the record of the rules in force to naming an exemption
// for what it does, and to changing its digest when a rule changes from holding to exempting.
func TestInForceDescribesAnExemption(t *testing.T) {
	t.Parallel()
	exempt := exemption("nightly smoke", func(p *Policy) { p.Tool = "bash" })
	hold := *exempt
	hold.Effect = EffectRequireApproval
	tests := []struct {
		// Policies are the rules described.
		Policies []*Policy
		// WantRules are the descriptions the record carries.
		WantRules []string
	}{{ // Test 0: An exemption says it lets an agent's run proceed.
		Policies:  []*Policy{exempt},
		WantRules: []string{"nightly smoke: lets an agent's run proceed without the default hold"},
	}, { // Test 1: The same rule holding says it requires approval, the control.
		Policies:  []*Policy{&hold},
		WantRules: []string{"nightly smoke: requires approval"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := InForce(test.Policies)
			if diff := cmp.Diff(test.WantRules, got.Rules, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("rules mismatch (-want +got):\n%s", diff)
			}
		})
	}
	if InForce([]*Policy{exempt}).Digest == InForce([]*Policy{&hold}).Digest {
		t.Error("an exemption and the same rule holding share a digest, so turning a hold into an " +
			"exemption would leave the record of the rules in force unchanged")
	}
}

// TestPlanGatedFollowsTheAgentHold holds a Terraform or OpenTofu apply the built-in hold covers to
// planning first, so the apply it proposes is what waits and the approval binds the saved plan.
func TestPlanGatedFollowsTheAgentHold(t *testing.T) {
	t.Parallel()
	exempt := exemption("tf bot", func(p *Policy) { p.Tool = "terraform" })
	tests := []struct {
		// Policies are the rules in force.
		Policies []*Policy
		// Run is the apply asked about.
		Run *run.Run
		// WantPlanned is whether it plans before it applies.
		WantPlanned bool
	}{{ // Test 0: An agent's apply plans first with no policy written.
		Run: agentRun("bot", "terraform", "infra"), WantPlanned: true,
	}, { // Test 1: A person's apply does not, the control.
		Run: personRun("terraform", "infra"), WantPlanned: false,
	}, { // Test 2: An exempt agent's apply applies as a person's would.
		Policies: []*Policy{exempt}, Run: agentRun("bot", "terraform", "infra"),
		WantPlanned: false,
	}, { // Test 3: An OpenTofu apply plans first the same way.
		Run: agentRun("bot", "opentofu", "infra"), WantPlanned: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := PlanGated(test.Policies, test.Run); got != test.WantPlanned {
				t.Errorf("PlanGated() = %v, want %v", got, test.WantPlanned)
			}
		})
	}
}

// TestAFileExemptionLoadsOnCommunity holds the policy file to letting a Community install write its
// one exemption, while the cap still counts it. It drops the package's Team license around itself,
// so it cannot run in parallel with the tests that read the license.
func TestAFileExemptionLoadsOnCommunity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policies.yml")
	team := license.Current()
	license.Set(nil)
	t.Cleanup(func() { license.Set(team) })

	const exempt = "  - name: nightly-smoke\n    tool: bash\n    command_contains: smoke\n" +
		"    actor_kind: agent\n    effect: exempt\n"
	tests := []struct {
		// Body is the policy file.
		Body string
		// WantRefused is whether reading it on Community is refused.
		WantRefused bool
		// WantEffects are the effects of the rules served when it is not refused.
		WantEffects []string
	}{{ // Test 0: One exemption, scoped to agents, is a Community policy file.
		Body: "policies:\n" + exempt, WantEffects: []string{EffectExempt},
	}, { // Test 1: The same rule holding instead is the full engine and is refused, the control.
		Body: "policies:\n" + strings.Replace(exempt, "effect: exempt", "effect: require_approval",
			1),
		WantRefused: true,
	}, { // Test 2: The exemption counts toward the one policy Community holds.
		Body:        "policies:\n" + exempt + "  - name: hold-everything\n",
		WantRefused: true,
	}}
	for testNum, test := range tests {
		if err := os.WriteFile(path, []byte(test.Body), 0o600); err != nil {
			t.Fatalf("test %d: write policy file: %v", testNum, err)
		}
		store, err := NewFileStore(path)
		if err == nil {
			var set []*Policy
			set, err = store.List(context.Background())
			if err == nil {
				var effects []string
				for _, p := range set {
					effects = append(effects, p.Effect)
				}
				if diff := cmp.Diff(test.WantEffects, effects, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("test %d: effects mismatch (-want +got):\n%s", testNum, diff)
				}
			}
		}
		if refused := err != nil; refused != test.WantRefused {
			t.Errorf("test %d: refused = %v (%v), want %v", testNum, refused, err, test.WantRefused)
			continue
		}
		// A refusal has to be the license's, or a file that failed to parse would pass as one.
		if test.WantRefused && !strings.Contains(err.Error(), "switchtender.com/pricing") {
			t.Errorf("test %d: refused for another reason: %v", testNum, err)
		}
	}
}
