package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAnAgentsWorkflowCannotApply covers the one way an agent could have traded the two approvals
// its apply takes for one that binds no plan: a workflow carrying the apply as a step. A
// workflow's approval binds its steps as written, and an apply step plans and applies when it
// runs, so an agent's workflow with a Terraform or OpenTofu apply step is refused at submission,
// with the refusal on the chain, unless an exemption covers the step as the run it would become.
// A plan step and an Ansible workflow are held as a whole as before, and a person's workflow with
// the same apply step is accepted, which are the controls.
//
//nolint:funlen // Test function.
func TestAnAgentsWorkflowCannotApply(t *testing.T) {
	t.Parallel()
	build := run.PipelineStep{Name: "build", Tool: run.ToolBash, Command: "make"}
	apply := run.PipelineStep{Name: "apply", Tool: run.ToolTerraform, Command: "infra",
		DependsOn: []string{"build"}}
	plan := apply
	plan.Name, plan.DryRun = "plan", true
	tofu := apply
	tofu.Name, tofu.Tool = "tofu", run.ToolOpenTofu
	gate := run.PipelineStep{Name: "gate", Type: run.StepApproval, DependsOn: []string{"build"}}
	afterGate := apply
	afterGate.DependsOn = []string{"gate"}
	stepOnly := &policy.Policy{ID: "pol_tf", Name: "lead applies", Tool: run.ToolTerraform,
		Actor: "release-agent", Account: "dev-lead", Effect: policy.EffectExempt,
		MaxDestroy: policy.DisabledMaxDestroy}
	whole := &policy.Policy{ID: "pol_all", Name: "lead agent", Actor: "release-agent",
		Account: "dev-lead", Effect: policy.EffectExempt, MaxDestroy: policy.DisabledMaxDestroy}
	otherAccount := &policy.Policy{ID: "pol_ops", Name: "ops applies", Tool: run.ToolTerraform,
		Actor: "release-agent", Account: "ops-lead", Effect: policy.EffectExempt,
		MaxDestroy: policy.DisabledMaxDestroy}
	tests := []struct {
		// Steps are the workflow.
		Steps []run.PipelineStep
		// Rules are the stored policies.
		Rules []*policy.Policy
		// Person submits as a person rather than as the agent.
		Person bool
		// DryRun submits the whole workflow as a dry run.
		DryRun bool
		// WantRefused names the step the refusal names, empty when the workflow is accepted.
		WantRefused string
		// WantHeld is, for an accepted workflow, whether it waits for a person.
		WantHeld bool
	}{{ // Test 0: A Terraform apply step is refused.
		Steps: []run.PipelineStep{build, apply}, WantRefused: "apply",
	}, { // Test 1: An OpenTofu apply step is refused.
		Steps: []run.PipelineStep{build, tofu}, WantRefused: "tofu",
	}, { // Test 2: An apply behind an approval step is refused all the same.
		Steps: []run.PipelineStep{build, gate, afterGate}, WantRefused: "apply",
	}, { // Test 3: An exemption for the same label on another account covers nothing here.
		Steps: []run.PipelineStep{build, apply}, Rules: []*policy.Policy{otherAccount},
		WantRefused: "apply",
	}, { // Test 4: A person's workflow with the same step is accepted, the control for test 0.
		Steps: []run.PipelineStep{build, apply}, Person: true,
	}, { // Test 5: A plan step is held as a whole, as before.
		Steps: []run.PipelineStep{build, plan}, WantHeld: true,
	}, { // Test 6: The apply step in a workflow submitted as a dry run is a plan, held as before.
		Steps: []run.PipelineStep{build, apply}, DryRun: true, WantHeld: true,
	}, { // Test 7: A workflow with no infrastructure step is held as a whole, as before.
		Steps: []run.PipelineStep{build}, WantHeld: true,
	}, { // Test 8: An exemption covering the apply step lets the workflow be submitted and held.
		Steps: []run.PipelineStep{build, apply}, Rules: []*policy.Policy{stepOnly},
		WantHeld: true,
	}, { // Test 9: An exemption covering the agent's whole workflow lets it run.
		Steps: []run.PipelineStep{build, apply}, Rules: []*policy.Policy{whole},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := run.NewMemStore()
			audits := audit.NewMemStore()
			d := New(store, okRunner(), zap.NewNop(), WithNoJanitor(), WithAudits(audits),
				WithPolicies(rulesHolding(t, test.Rules...)))
			t.Cleanup(d.Close)
			ctx, opts := boundAgent("dev-lead")
			if test.Person {
				ctx, opts = context.Background(), personOpts()
			}
			if test.DryRun {
				opts = append(opts, run.WithDryRun(true))
			}
			steps := append([]run.PipelineStep(nil), test.Steps...)
			got, err := d.SubmitPipeline(ctx, "release", "", steps, opts...)
			if test.WantRefused != "" {
				if !errors.Is(err, ErrPolicyDenied) || !errors.Is(err, ErrAgentWorkflowApply) {
					t.Fatalf("SubmitPipeline() error = %v, want the agent workflow refusal", err)
				}
				for _, want := range []string{`step "` + test.WantRefused + `"`,
					"as its own run", "effect exempt"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal %q does not say %q", err, want)
					}
				}
				assertRefusalRecorded(t, audits)
				if runs, lerr := store.List(context.Background()); lerr != nil || len(runs) != 0 {
					t.Errorf("a refused workflow stored %d runs (%v), want none", len(runs), lerr)
				}
				return
			}
			if err != nil {
				t.Fatalf("SubmitPipeline() error = %v, want the workflow accepted", err)
			}
			if held := got.Status == run.StatusPendingApproval; held != test.WantHeld {
				t.Errorf("held = %v, want %v (held by %q)", held, test.WantHeld, got.HeldByPolicy)
			}
		})
	}
}

// assertRefusalRecorded fails unless the chain holds the refusal of a workflow by the built-in
// rule, naming it in the entry's path, the way a deny rule's refusal is recorded.
func assertRefusalRecorded(t *testing.T, audits audit.Store) {
	t.Helper()
	entries, err := audits.List(context.Background(), 100)
	if err != nil {
		t.Fatalf("audits.List() error = %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Path, "/decision/refused/policy/"+policy.AgentWorkflowApplyName) {
			return
		}
	}
	t.Errorf("no refusal by %q was recorded on the chain", policy.AgentWorkflowApplyName)
}
