package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// mustCompileRego compiles body as the only module of package switchtender.
func mustCompileRego(t *testing.T, body string) *RegoProgram {
	t.Helper()
	prog, err := CompileRego("", "", []RegoModule{{
		File: "policy.rego", Source: "package switchtender\n\n" + body + "\n",
	}})
	if err != nil {
		t.Fatalf("CompileRego() error = %v", err)
	}
	return prog
}

// regoPolicy wraps a compiled body in a policy the way the file store does.
func regoPolicy(t *testing.T, name, body string) *Policy {
	t.Helper()
	return &Policy{
		ID: filePolicyID(name), Name: name, MaxDestroy: DisabledMaxDestroy,
		Rego: mustCompileRego(t, body),
	}
}

// verdict is everything the dispatcher reads from a policy set about one run.
type verdict struct {
	// Denied is whether the submission is refused.
	Denied bool
	// Held is whether the run waits for approval, at submission or after its plan.
	Held bool
	// Distinct is whether its approver must not be its requester.
	Distinct bool
	// PlanGated is whether an apply is planned before it applies.
	PlanGated bool
}

// verdictOf asks a policy set every question the dispatcher asks, in the order it asks them. A
// plan-content YAML rule holds a proposed apply through Exceeding, while a Rego policy holds it
// through Requiring on the apply carrying the plan's count, so both are read as holds.
func verdictOf(policies []*Policy, r *run.Run) verdict {
	v := verdict{Denied: Denying(policies, r) != nil}
	if v.Denied {
		return v
	}
	v.Held = Requiring(policies, r) != nil
	v.Distinct = RequireDistinct(policies, r)
	if r.PlanDestroys != nil {
		if Exceeding(policies, r, *r.PlanDestroys) != nil {
			v.Held = true
			v.Distinct = v.Distinct || ExceedingDistinct(policies, r, *r.PlanDestroys)
		}
	}
	tool := run.NormalizeTool(r.Tool)
	if (tool == run.ToolTerraform || tool == run.ToolOpenTofu) && !r.DryRun && r.ProposedFrom == "" {
		v.PlanGated = PlanGated(policies, r)
	}
	return v
}

// parityRuns are the runs every parity case is judged on: a spread of tools, actors, queues,
// grades, and plan states, so a Rego rule and its YAML twin have to agree on all of them.
func parityRuns() []*run.Run {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	seven, two := 7, 2
	return []*run.Run{
		{ID: "r0", Tool: "terraform", Command: "infra/prod DESTROY", CreatedAt: at},
		{ID: "r1", Tool: "terraform", Command: "infra/prod", CreatedAt: at},
		{ID: "r2", Tool: "terraform", Command: "infra/prod", DryRun: true, CreatedAt: at},
		{ID: "r3", Tool: "bash", Command: "rm -rf /var/lib/db", ActorType: "agent",
			Actor: "deploy-bot", CreatedAt: at},
		{ID: "r4", Tool: "bash", Command: "uptime", ActorType: "agent", Actor: "deploy-bot",
			CreatedAt: at},
		{ID: "r5", Tool: "bash", Command: "rm -rf /var/lib/db", ActorType: "session",
			Actor: "operator", CreatedAt: at},
		{ID: "r6", Playbook: "site.yml", Inventory: "hosts", InventoryID: "inv_prod",
			Queue: "prod", CreatedAt: at},
		{ID: "r7", Playbook: "site.yml", Inventory: "hosts", InventoryID: "inv_dev",
			Queue: "dev", DryRun: true, CreatedAt: at},
		{ID: "r8", Tool: "opentofu", Command: "infra/net", ProposedFrom: "r1",
			PlanDestroys: &seven, CreatedAt: at},
		{ID: "r9", Tool: "opentofu", Command: "infra/net", ProposedFrom: "r1",
			PlanDestroys: &two, CreatedAt: at},
		{ID: "r10", Tool: "opentofu", Command: "infra/net", CreatedAt: at},
		{ID: "r11", Tool: "python", Command: "print(1)", ActorType: "webhook", CreatedAt: at},
		{ID: "r12", Playbook: "site.yml", Queue: "prod", DryRun: true, CreatedAt: at},
		{ID: "r13", Playbook: "site.yml", Queue: "prod", DryRun: true, CreatedAt: at,
			DryRunScans: []run.DryRunScan{{Tool: "ansible",
				Findings: []string{`site.yml: task "Restart" sets check_mode to false`}}}},
		{ID: "r14", Tool: "terraform", Command: "infra/prod", Queue: "prod", DryRun: true,
			CreatedAt: at, DryRunScans: []run.DryRunScan{{Tool: "terraform",
				Findings: []string{"data.external.x runs a program during plan (main.tf line 1)"}}}},
		{ID: "r15", Tool: "opentofu", Command: "infra/prod", Queue: "prod", DryRun: true,
			CreatedAt: at, DryRunScans: []run.DryRunScan{{Tool: "opentofu",
				Unread: []string{`module.vpc from "acme/vpc/aws" (not downloaded here)`}}}},
	}
}

// TestARegoDryRunExemptionHoldsAForcingDryRunLikeYAML pins that a Rego module exempting previews
// by run.dry_run, the way porters write a YAML exclude_dry_run rule, gives the YAML rule's verdict
// on both kinds of dry run: a change-free one passes under both, and one the gate did not find
// change free is held under both, whether its playbook forces real tasks under check mode, its
// configuration runs a program while it plans, or the scan could not read all of it.
func TestARegoDryRunExemptionHoldsAForcingDryRunLikeYAML(t *testing.T) {
	t.Parallel()
	yaml := &Policy{ID: "pol_yaml", Name: "prod queue", Queue: "prod", ExcludeDryRun: true,
		MaxDestroy: -1}
	rego := regoPolicy(t, "prod queue", `hold contains "production" if {
	input.run.queue == "prod"
	not input.run.dry_run
}`)
	direct := regoPolicy(t, "prod queue direct", `hold contains "production" if {
	input.run.queue == "prod"
	not input.run.change_free
}`)
	tests := []struct {
		// Tool is the dry run's tool.
		Tool string
		// Scans are what the gate's scan of the dry run read.
		Scans []run.DryRunScan
		// WantHeld is the verdict every engine must give.
		WantHeld bool
	}{{ // Test 0: A change-free dry run passes.
		Tool: "ansible", WantHeld: false,
	}, { // Test 1: A dry run that forces real tasks is held.
		Tool: "ansible", Scans: []run.DryRunScan{{Tool: "ansible",
			Findings: []string{`site.yml: task "Restart" sets check_mode to false`}}},
		WantHeld: true,
	}, { // Test 2: A plan that runs a program is held.
		Tool: "terraform", Scans: []run.DryRunScan{{Tool: "terraform",
			Findings: []string{"data.external.x runs a program during plan (main.tf line 1)"}}},
		WantHeld: true,
	}, { // Test 3: A plan the scan could not read in full is held.
		Tool: "opentofu", Scans: []run.DryRunScan{{Tool: "opentofu",
			Unread: []string{`module.vpc from "acme/vpc/aws" (not downloaded here)`}}},
		WantHeld: true,
	}, { // Test 4: A plan read in full that runs nothing passes.
		Tool: "terraform", Scans: []run.DryRunScan{{Tool: "terraform", Inputs: []string{"main.tf"}}},
		WantHeld: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := &run.Run{ID: "run_dry", Tool: test.Tool, Playbook: "site.yml", Command: "infra",
				Queue: "prod", DryRun: true, DryRunScans: test.Scans, CreatedAt: time.Now()}
			got := map[string]bool{
				"yaml":   Requiring([]*Policy{yaml}, r) != nil,
				"rego":   Requiring([]*Policy{rego}, r) != nil,
				"direct": Requiring([]*Policy{direct}, r) != nil,
			}
			want := map[string]bool{"yaml": test.WantHeld, "rego": test.WantHeld,
				"direct": test.WantHeld}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("held mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestARegoInputStatesWhatADryRunForces pins that the input keeps the tool flag in dry_run and
// says separately what the gate's scan found and whether the run is change free, for every tool
// the gate scans.
func TestARegoInputStatesWhatADryRunForces(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Run            *run.Run
		WantChangeFree bool
		WantFindings   []any
	}{{ // Test 0: An Ansible dry run whose playbook forces a task.
		Run: &run.Run{ID: "run_dry", Playbook: "site.yml", DryRun: true,
			DryRunScans: []run.DryRunScan{{Tool: "ansible",
				Findings: []string{`site.yml: task "Restart" sets check_mode to false`}}}},
		WantFindings: []any{`site.yml: task "Restart" sets check_mode to false`},
	}, { // Test 1: A plan whose configuration could not be read in full.
		Run: &run.Run{ID: "run_plan", Tool: "terraform", Command: "infra", DryRun: true,
			DryRunScans: []run.DryRunScan{{Tool: "terraform",
				Unread: []string{`module.vpc from "acme/vpc/aws" (not downloaded here)`}}}},
		WantFindings: []any{`could not read module.vpc from "acme/vpc/aws" (not downloaded here), ` +
			`so it may run a program during plan`},
	}, { // Test 2: A change-free plan says so, with nothing found.
		Run: &run.Run{ID: "run_clean", Tool: "terraform", Command: "infra", DryRun: true,
			DryRunScans: []run.DryRunScan{{Tool: "terraform", Inputs: []string{"main.tf"}}}},
		WantChangeFree: true, WantFindings: []any{},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			in, ok := RegoInput(test.Run)["run"].(map[string]any)
			if !ok {
				t.Fatal("the input has no run object")
			}
			got := map[string]any{"dry_run": in["dry_run"], "change_free": in["change_free"],
				"dry_run_findings": in["dry_run_findings"]}
			want := map[string]any{"dry_run": true, "change_free": test.WantChangeFree,
				"dry_run_findings": test.WantFindings}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("input mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestARegoRuleNamesTheTemplateHoweverTheRunWasFired pins run.template_id: a scheduled or
// triggered run names its schedule or trigger in source_id, so a rule scoped to one job by
// source_id missed every fire that did not launch the template directly. template_id names the
// template whatever fired it, and a module reading it is accepted at load.
func TestARegoRuleNamesTheTemplateHoweverTheRunWasFired(t *testing.T) {
	t.Parallel()
	scoped := regoPolicy(t, "one job", `hold contains "the nightly purge" if {
	input.run.template_id == "tpl_purge"
}`)
	tests := []struct {
		Run          *run.Run
		WantTemplate string
		WantHeld     bool
	}{{ // Test 0: Launched directly, source_id and template_id agree.
		Run: &run.Run{ID: "run_1", Source: "template", SourceID: "tpl_purge",
			TemplateID: "tpl_purge"},
		WantTemplate: "tpl_purge", WantHeld: true,
	}, { // Test 1: Fired by a schedule, source_id names the schedule and template_id the job.
		Run: &run.Run{ID: "run_2", Source: "schedule", SourceID: "sch_nightly",
			TemplateID: "tpl_purge"},
		WantTemplate: "tpl_purge", WantHeld: true,
	}, { // Test 2: Fired by a trigger.
		Run: &run.Run{ID: "run_3", Source: "trigger", SourceID: "trg_push",
			TemplateID: "tpl_purge"},
		WantTemplate: "tpl_purge", WantHeld: true,
	}, { // Test 3: Another template's run is not caught.
		Run: &run.Run{ID: "run_4", Source: "schedule", SourceID: "sch_nightly",
			TemplateID: "tpl_backup"},
		WantTemplate: "tpl_backup", WantHeld: false,
	}, { // Test 4: A run no template launched names none.
		Run: &run.Run{ID: "run_5", Source: "api"}, WantTemplate: "", WantHeld: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			in, ok := RegoInput(test.Run)["run"].(map[string]any)
			if !ok {
				t.Fatal("the input has no run object")
			}
			if diff := cmp.Diff(test.WantTemplate, in["template_id"]); diff != "" {
				t.Errorf("run.template_id mismatch (-want +got):\n%s", diff)
			}
			if held := Requiring([]*Policy{scoped}, test.Run) != nil; held != test.WantHeld {
				t.Errorf("held = %t, want %t", held, test.WantHeld)
			}
		})
	}
}

// TestARegoRuleDecidesExactlyAsItsYAMLTwin pins the promise that a Rego decision behaves like a
// YAML one downstream. Each case pairs a YAML rule with the Rego a porter would write for it, and
// the two have to give the dispatcher the same answer to every question on every run: refuse, hold,
// demand a second approver, plan first.
func TestARegoRuleDecidesExactlyAsItsYAMLTwin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the rule pair.
		Name string
		// YAML is the criteria rule.
		YAML Policy
		// Rego is the body of the equivalent module.
		Rego string
	}{{ // Test 0: A blanket hold on a tool and a command, matched without regard to case.
		Name: "terraform destroy",
		YAML: Policy{Tool: "terraform", CommandContains: "destroy", MaxDestroy: -1},
		Rego: `hold contains "terraform destroy" if {
	input.run.tool == "terraform"
	contains(lower(input.run.command), "destroy")
}`,
	}, { // Test 1: A deny scoped to agents at a risk floor.
		Name: "agents never high risk",
		YAML: Policy{ActorKind: ActorKindAgent, MinRisk: run.RiskHigh, Effect: EffectDeny,
			MaxDestroy: -1},
		Rego: `deny contains "agents may not make high risk changes" if {
	input.actor.kind == "agent"
	input.risk.level == "high"
}`,
	}, { // Test 2: Separation of duties on anything that cannot be undone.
		Name: "irreversible needs two people",
		YAML: Policy{Reversibility: run.Irreversible, RequireDistinctApprover: true, MaxDestroy: -1},
		Rego: `hold contains "irreversible" if input.reversibility.class == "irreversible"

require_distinct_approver if input.reversibility.class == "irreversible"

plan_gate if input.reversibility.class != "irreversible"`,
	}, { // Test 3: A production queue held except for previews.
		Name: "prod queue",
		YAML: Policy{Queue: "prod", ExcludeDryRun: true, MaxDestroy: -1},
		Rego: `hold contains "production" if {
	input.run.queue == "prod"
	not input.run.dry_run
}`,
	}, { // Test 4: A destroy threshold, which plans the apply and holds the one that crosses it.
		Name: "large teardown",
		YAML: Policy{Tool: "opentofu", MaxDestroy: 5},
		Rego: `plan_gate if input.run.tool == "opentofu"

hold contains msg if {
	input.run.tool == "opentofu"
	input.plan.planned
	input.plan.destroys > 5
	msg := sprintf("plan destroys %d", [input.plan.destroys])
}`,
	}, { // Test 5: One named principal and one inventory.
		Name: "bot on prod inventory",
		YAML: Policy{Actor: "deploy-bot", Tool: "bash", MaxDestroy: -1},
		Rego: `hold contains "deploy-bot" if {
	input.actor.name == "deploy-bot"
	input.run.tool == "bash"
}`,
	}, { // Test 6: A stored inventory, matched by id.
		Name: "prod inventory",
		YAML: Policy{InventoryID: "inv_prod", MaxDestroy: -1},
		Rego: `hold contains "prod inventory" if input.run.inventory.id == "inv_prod"`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			yaml := test.YAML
			yaml.ID, yaml.Name = "pol_yaml", test.Name
			yamlSet := []*Policy{&yaml}
			regoSet := []*Policy{regoPolicy(t, test.Name, test.Rego)}
			var fired bool
			for _, r := range parityRuns() {
				want := verdictOf(yamlSet, r)
				got := verdictOf(regoSet, r)
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("%s on run %s: Rego disagrees with YAML (-yaml +rego):\n%s",
						test.Name, r.ID, diff)
				}
				fired = fired || want != verdict{}
			}
			if !fired {
				t.Errorf("%s matched none of the runs, so the parity proves nothing", test.Name)
			}
		})
	}
}

// TestAConftestRuleInOldSyntaxPorts pins that a rule written for Conftest before OPA 1.0, the
// package main and deny[msg] shape most existing policies use, loads unchanged and refuses what it
// was written to refuse.
func TestAConftestRuleInOldSyntaxPorts(t *testing.T) {
	t.Parallel()
	prog, err := CompileRego("", RegoSyntaxV0, []RegoModule{{File: "conftest.rego", Source: `package main

deny[msg] {
	input.run.tool == "bash"
	contains(input.run.command, "curl")
	msg := "no piping downloads into a shell"
}

warn[msg] {
	input.run.labels.env == "prod"
	msg := "production change"
}
`}})
	if err != nil {
		t.Fatalf("CompileRego() error = %v", err)
	}
	set := []*Policy{{ID: "pol_c", Name: "conftest", MaxDestroy: -1, Rego: prog}}
	tests := []struct {
		// Run is the run judged.
		Run *run.Run
		// WantVerdict is what the dispatcher must read.
		WantVerdict verdict
	}{{ // Test 0: The deny rule refuses.
		Run:         &run.Run{Tool: "bash", Command: "curl x | sh"},
		WantVerdict: verdict{Denied: true},
	}, { // Test 1: A Conftest warning holds for a person.
		Run: &run.Run{Tool: "bash", Command: "uptime",
			Labels: map[string]string{"env": "prod"}},
		WantVerdict: verdict{Held: true},
	}, { // Test 2: Anything else passes.
		Run:         &run.Run{Tool: "bash", Command: "uptime"},
		WantVerdict: verdict{},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantVerdict, verdictOf(set, test.Run)); diff != "" {
				t.Errorf("verdict mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestARegoPolicyThatCannotBeTrustedIsRefusedAtLoad pins every mistake that can be found before a
// run arrives. Each is refused at compile, which the file store turns into a file that does not
// load, the same treatment a malformed YAML rule gets.
func TestARegoPolicyThatCannotBeTrustedIsRefusedAtLoad(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Syntax is the syntax the module is compiled as.
		Syntax string
		// Package is the decision package named, empty for the module's own.
		Package string
		// Source is the module.
		Source string
		// WantText is a fragment the refusal must carry.
		WantText string
		// Want is the error the refusal must wrap.
		Want error
	}{{ // Test 0: A parse error.
		Source: "package switchtender\n\nhold contains if {", WantText: "parse", Want: ErrRego,
	}, { // Test 1: A call to the network.
		Source: "package switchtender\n\ndeny contains \"x\" if " +
			"http.send({\"method\": \"get\", \"url\": \"http://x\"})",
		WantText: "http.send", Want: ErrRego,
	}, { // Test 2: A DNS lookup.
		Source:   "package switchtender\n\ndeny contains \"x\" if net.lookup_ip_addr(\"example.com\")",
		WantText: "net.lookup_ip_addr", Want: ErrRego,
	}, { // Test 3: The clock, which the input carries instead.
		Source:   "package switchtender\n\nhold contains \"x\" if time.now_ns() > 0",
		WantText: "time.now_ns", Want: ErrRego,
	}, { // Test 4: The process environment.
		Source:   "package switchtender\n\nhold contains \"x\" if opa.runtime().env.HOME",
		WantText: "opa.runtime", Want: ErrRego,
	}, { // Test 5: A misspelled input field, which would otherwise never fire.
		Source:   "package switchtender\n\ndeny contains \"x\" if input.run.toool == \"bash\"",
		WantText: "input.run.toool", Want: ErrRego,
	}, { // Test 6: A field under a value that has none.
		Source:   "package switchtender\n\ndeny contains \"x\" if input.run.tool.name == \"bash\"",
		WantText: "has no field", Want: ErrRego,
	}, { // Test 7: A package that decides nothing at all.
		Source:   "package switchtender\n\nhelper := true",
		WantText: "defines none", Want: ErrRego,
	}, { // Test 8: A decision package nothing declares.
		Package: "data.elsewhere", Source: "package switchtender\n\nhold contains \"x\" if true",
		WantText: "defines none", Want: ErrRego,
	}, { // Test 9: A plan reader that never asks for a plan.
		Source:   "package switchtender\n\nhold contains \"x\" if input.plan.destroys > 1",
		WantText: "plan_gate", Want: ErrRego,
	}, { // Test 10: A syntax this build does not know.
		Syntax: "v2", Source: "package switchtender\n\nhold contains \"x\" if true",
		WantText: "syntax", Want: ErrRego,
	}, { // Test 11: Old syntax compiled as new.
		Source:   "package main\n\ndeny[msg] {\n\tmsg := \"x\"\n}",
		WantText: "parse", Want: ErrRego,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			_, err := CompileRego(test.Package, test.Syntax,
				[]RegoModule{{File: "bad.rego", Source: test.Source}})
			if !errors.Is(err, test.Want) {
				t.Fatalf("CompileRego() error = %v, want %v", err, test.Want)
			}
			if !strings.Contains(err.Error(), test.WantText) {
				t.Errorf("CompileRego() error = %v, want it to mention %q", err, test.WantText)
			}
		})
	}
	if d := (&RegoProgram{}).Decide(&run.Run{}); !errors.Is(d.Err, ErrRego) {
		t.Errorf("Decide() on an uncompiled program error = %v, want ErrRego", d.Err)
	}
	if _, err := CompileRego("", "", nil); !errors.Is(err, ErrRego) {
		t.Errorf("CompileRego(no modules) error = %v, want ErrRego", err)
	}
}

// TestARegoPolicyThatCannotDecideRefuses pins the runtime half of failing closed. A policy that
// errors, runs out of time, or answers in a shape that cannot be read refuses the submission,
// holds it if asked to, demands a second approver, and plans an apply, so no question the
// dispatcher asks it is answered with a pass.
func TestARegoPolicyThatCannotDecideRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Body is the module body.
		Body string
		// Timeout overrides the evaluation limit when set.
		Timeout time.Duration
		// NilRun judges no run at all instead of a Terraform apply.
		NilRun bool
		// WantText is a fragment the refusal's label must carry.
		WantText string
	}{{ // Test 0: Two values for one complete rule.
		Body:     "hold := \"a\" if true\n\nhold := \"b\" if true",
		WantText: "conflict",
	}, { // Test 1: A builtin error, which strict evaluation surfaces rather than hides.
		Body:     "deny contains x if { x := to_number(\"abc\") }",
		WantText: "to_number",
	}, { // Test 2: A deny rule that is neither messages nor a boolean.
		Body:     "deny := 5",
		WantText: "deny is a",
	}, { // Test 3: An allow rule that is not a boolean.
		Body:     "allow := \"yes\"",
		WantText: "allow is a",
	}, { // Test 4: A distinct-approver rule that is not a boolean.
		Body:     "hold contains \"x\" if false\n\nrequire_distinct_approver := \"always\"",
		WantText: "require_distinct_approver is a",
	}, { // Test 5: An evaluation that runs past its limit.
		Body: "hold contains \"x\" if {\n\tcount([y | some y in numbers.range(1, 50000000); " +
			"y < 0]) > 0\n}",
		Timeout:  20 * time.Millisecond,
		WantText: "exceeded",
	}, { // Test 6: No run to judge.
		Body: "hold contains \"x\" if true", NilRun: true, WantText: "no run",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			p := regoPolicy(t, "breaks", test.Body)
			if test.Timeout > 0 {
				p.Rego.timeout = test.Timeout
			}
			var r *run.Run
			if !test.NilRun {
				r = &run.Run{ID: "r", Tool: run.ToolTerraform, Command: "infra", CreatedAt: time.Now()}
			}
			set := []*Policy{p}
			denied := Denying(set, r)
			if denied == nil {
				t.Fatal("a policy that could not decide let the submission through")
			}
			if !strings.Contains(denied.Label(), test.WantText) {
				t.Errorf("refusal %q does not say why: want %q", denied.Label(), test.WantText)
			}
			if !strings.Contains(denied.Label(), p.Rego.Digest()[:12]) {
				t.Errorf("refusal %q does not name the bundle that refused", denied.Label())
			}
			if Requiring(set, r) == nil {
				t.Error("a policy that could not decide did not hold the run either")
			}
			if !RequireDistinct(set, r) {
				t.Error("a policy that could not decide let the requester approve")
			}
			if r != nil && !PlanGated(set, r) {
				t.Error("a policy that could not decide let an unplanned apply through")
			}
		})
	}
}

// TestAnAllowRuleRefusesWhatItDoesNotAllow pins the OPA allowlist convention: an allow rule that
// does not hold, including one left undefined with no default, refuses the run.
func TestAnAllowRuleRefusesWhatItDoesNotAllow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Body is the module body.
		Body string
		// Run is the run judged.
		Run *run.Run
		// WantDenied is whether the run is refused.
		WantDenied bool
	}{{ // Test 0: Allowed.
		Body: "allow if input.run.tool == \"ansible\"", Run: &run.Run{Playbook: "site.yml"},
		WantDenied: false,
	}, { // Test 1: Undefined, with no default, refuses.
		Body: "allow if input.run.tool == \"ansible\"", Run: &run.Run{Tool: "bash", Command: "x"},
		WantDenied: true,
	}, { // Test 2: A default of false refuses.
		Body:       "default allow := false\n\nallow if input.run.tool == \"ansible\"",
		Run:        &run.Run{Tool: "bash", Command: "x"},
		WantDenied: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := Denying([]*Policy{regoPolicy(t, "allowlist", test.Body)}, test.Run) != nil
			if got != test.WantDenied {
				t.Errorf("denied = %v, want %v", got, test.WantDenied)
			}
		})
	}
}

// TestRegoReachesNothingOutsideTheProcess pins that evaluation is offline and deterministic. No
// builtin OPA marks nondeterministic is callable, nor any on the disabled list, no network host is
// allowed, and a module aimed at a live server is refused before it can send a byte.
func TestRegoReachesNothingOutsideTheProcess(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"deny": false}`))
	}))
	defer srv.Close()

	for _, syntax := range []string{RegoSyntaxV1, RegoSyntaxV0} {
		caps := regoCapabilities(syntax)
		if caps.AllowNet == nil || len(caps.AllowNet) != 0 {
			t.Errorf("%s: AllowNet = %v, want an empty list, which allows no host", syntax,
				caps.AllowNet)
		}
		for _, b := range caps.Builtins {
			if b.Nondeterministic || slices.Contains(regoDisabledBuiltins, b.Name) {
				t.Errorf("%s: builtin %s is callable", syntax, b.Name)
			}
		}
	}

	src := fmt.Sprintf(`package switchtender

allow if {
	resp := http.send({"method": "get", "url": %q})
	resp.body.deny == false
}`, srv.URL)
	if _, err := CompileRego("", "", []RegoModule{{File: "net.rego", Source: src}}); err == nil {
		t.Fatal("a module calling http.send compiled")
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the server saw %d requests from a policy that should never reach it", n)
	}
}

// TestRegoInputCarriesEveryDocumentedField holds the schema the load-time check uses to the
// document evaluation builds, in both directions, so a field cannot be added to one and not the
// other. It also pins that a run with nothing set still yields every field.
func TestRegoInputCarriesEveryDocumentedField(t *testing.T) {
	t.Parallel()
	got := schemaPaths("input", anyShape(RegoInput(&run.Run{})))
	want := schemaPaths("input", regoInputSchema)
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("input document and schema disagree (-schema +document):\n%s", diff)
	}
	in := RegoInput(&run.Run{Tool: "", PlanDestroys: nil})
	if tool := in["run"].(map[string]any)["tool"]; tool != run.ToolAnsible {
		t.Errorf("run.tool = %v, want the normalized tool", tool)
	}
	plan := in["plan"].(map[string]any)
	if plan["planned"] != false || plan["destroys"] != nil {
		t.Errorf("plan = %v, want unplanned with no destroy count", plan)
	}
	if in["version"] != RegoInputVersion {
		t.Errorf("version = %v, want %d", in["version"], RegoInputVersion)
	}
}

// anyShape converts a document to the schema's vocabulary: objects stay objects, everything else is
// a leaf, and labels are an open object.
func anyShape(doc map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range doc {
		if k == "labels" {
			out[k] = regoAnyKeys
			continue
		}
		if m, ok := v.(map[string]any); ok {
			out[k] = anyShape(m)
			continue
		}
		out[k] = nil
	}
	return out
}

// schemaPaths lists every path in a schema-shaped document, sorted.
func schemaPaths(prefix string, node map[string]any) []string {
	var out []string
	for k, v := range node {
		path := prefix + "." + k
		out = append(out, path)
		if m, ok := v.(map[string]any); ok {
			out = append(out, schemaPaths(path, m)...)
		}
	}
	sort.Strings(out)
	return out
}

// TestARegoDecisionNamesTheBundleThatDecided pins the evidence. The rule set's digest, which every
// run carries into its outcome record, must move with any edit to what a Rego policy decides and
// stay put for a rename, and the record must name the bundle digest in full.
func TestARegoDecisionNamesTheBundleThatDecided(t *testing.T) {
	t.Parallel()
	base := func() []RegoModule {
		return []RegoModule{{
			File: "a.rego", Source: "package switchtender\n\nhold contains \"x\" if true\n",
		}}
	}
	compile := func(pkg string, modules []RegoModule) *RegoProgram {
		prog, err := CompileRego(pkg, "", modules)
		if err != nil {
			t.Fatalf("CompileRego() error = %v", err)
		}
		return prog
	}
	digestOf := func(name string, prog *RegoProgram) string {
		return InForce([]*Policy{{ID: "pol", Name: name, MaxDestroy: -1, Rego: prog}}).Digest
	}
	want := digestOf("gate", compile("", base()))
	tests := []struct {
		// Name labels the policy.
		Name string
		// Modules are the edited modules.
		Modules []RegoModule
		// WantMoved is whether the set digest must move.
		WantMoved bool
	}{{ // Test 0: A rename does not change what it decides.
		Name: "renamed", Modules: base(), WantMoved: false,
	}, { // Test 1: An edited message is an edited policy.
		Name: "gate", Modules: []RegoModule{{File: "a.rego",
			Source: "package switchtender\n\nhold contains \"y\" if true\n"}}, WantMoved: true,
	}, { // Test 2: A moved file is visible.
		Name: "gate", Modules: []RegoModule{{File: "b.rego", Source: base()[0].Source}},
		WantMoved: true,
	}, { // Test 3: An added helper module is visible.
		Name: "gate", Modules: append(base(), RegoModule{File: "lib.rego",
			Source: "package lib\n\nx := 1\n"}), WantMoved: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			moved := digestOf(test.Name, compile("", test.Modules)) != want
			if moved != test.WantMoved {
				t.Errorf("digest moved = %v, want %v", moved, test.WantMoved)
			}
		})
	}

	prog := compile("", base())
	set := InForce([]*Policy{{ID: "pol", Name: "gate", MaxDestroy: -1, Rego: prog}})
	wantRule := "gate: decided by Rego package data.switchtender, bundle sha256:" + prog.Digest()
	if diff := cmp.Diff([]string{wantRule}, set.Rules); diff != "" {
		t.Errorf("rule description mismatch (-want +got):\n%s", diff)
	}
	held := Requiring([]*Policy{{ID: "pol", Name: "gate", MaxDestroy: -1, Rego: prog}},
		&run.Run{ID: "r", Playbook: "site.yml"})
	if held == nil {
		t.Fatal("the policy did not hold")
	}
	if wantLabel := "gate (x, rego sha256:" + prog.Digest()[:12] + ")"; held.Label() != wantLabel {
		t.Errorf("held by %q, want %q", held.Label(), wantLabel)
	}
}

// TestARegoPolicyTravelsToAWorkerIntact pins the relay path. A worker reads the control node's
// policies as JSON, so a Rego policy has to arrive compiled and deciding the same way, and one whose
// sources do not match the digest they claim has to fail to decode rather than arrive as an empty
// rule that holds everything or nothing.
func TestARegoPolicyTravelsToAWorkerIntact(t *testing.T) {
	t.Parallel()
	sent := regoPolicy(t, "travels", `deny contains "agents" if input.actor.kind == "agent"`)
	raw, err := json.Marshal([]*Policy{sent})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var got []*Policy
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got[0].Rego == nil || got[0].Rego.Digest() != sent.Rego.Digest() {
		t.Fatalf("the program did not arrive intact: %+v", got[0])
	}
	for _, r := range parityRuns() {
		if (Denying(got, r) != nil) != (Denying([]*Policy{sent}, r) != nil) {
			t.Errorf("run %s: the worker's copy decides differently", r.ID)
		}
	}

	tests := []struct {
		// Tamper edits the encoded policy.
		Tamper func(s string) string
	}{{ // Test 0: A different module under the original digest.
		Tamper: func(s string) string { return strings.Replace(s, `\"agents\"`, `\"people\"`, 1) },
	}, { // Test 1: A different digest over the original module.
		Tamper: func(s string) string {
			return strings.Replace(s, sent.Rego.Digest(), strings.Repeat("0", 64), 1)
		},
	}, { // Test 2: A module that no longer compiles.
		Tamper: func(s string) string {
			return strings.Replace(s, "deny contains", "deny contains if", 1)
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			tampered := test.Tamper(string(raw))
			if tampered == string(raw) {
				t.Fatal("the tamper changed nothing, so the case proves nothing")
			}
			var out []*Policy
			if err := json.Unmarshal([]byte(tampered), &out); !errors.Is(err, ErrRego) {
				t.Errorf("Unmarshal(tampered) error = %v, want ErrRego", err)
			}
		})
	}
}

// TestARegoPolicyRefusesCriteriaBesideIt pins that a Rego policy cannot also carry YAML criteria,
// which would read as narrowing it while changing nothing.
func TestARegoPolicyRefusesCriteriaBesideIt(t *testing.T) {
	t.Parallel()
	p := regoPolicy(t, "rego", `hold contains "x" if true`)
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate() error = %v on a plain Rego policy", err)
	}
	p.Tool = "bash"
	if err := p.Validate(); !errors.Is(err, ErrRego) {
		t.Errorf("Validate() error = %v, want ErrRego for a Rego policy with a tool criterion", err)
	}
}

// TestRegoEvaluatesSafelyInParallel runs one compiled program from many goroutines, as concurrent
// submissions do, so the race detector sees any shared state the prepared query keeps.
func TestRegoEvaluatesSafelyInParallel(t *testing.T) {
	t.Parallel()
	p := regoPolicy(t, "shared", `hold contains "bash" if input.run.tool == "bash"`)
	done := make(chan bool)
	for i := range 16 {
		go func(i int) {
			tool := "bash"
			if i%2 == 0 {
				tool = "python"
			}
			d := p.Rego.Decide(&run.Run{ID: fmt.Sprint(i), Tool: tool, Command: "x"})
			done <- d.Err == nil && (len(d.Hold) > 0) == (tool == "bash")
		}(i)
	}
	for range 16 {
		if !<-done {
			t.Error("a concurrent evaluation decided wrongly")
		}
	}
}
