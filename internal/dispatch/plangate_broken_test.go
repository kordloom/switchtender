package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/plantest"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// brokenPlanRunner fails or errors on the planning pass and counts any real apply that slipped past.
type brokenPlanRunner struct {
	// applies counts executions that were not dry runs, which are the real applies.
	applies atomic.Int64
	// output is written before the plan reports its result.
	output string
	// exitCode is what the plan pass exits with.
	exitCode int
	// err is what the plan pass returns, standing in for a tool that could not be launched.
	err error
	// plan, when set, is what a plan pass that succeeds returns, so a test chooses the saved plan.
	plan *roundhouse.Result
}

// Run reports the configured plan failure and counts any apply that reached it.
func (b *brokenPlanRunner) Run(_ context.Context, spec roundhouse.Spec,
	out io.Writer) (roundhouse.Result, error) {
	if !spec.DryRun {
		b.applies.Add(1)
		return roundhouse.Result{ExitCode: 0}, nil
	}
	_, _ = io.WriteString(out, b.output)
	if b.plan != nil && b.err == nil && b.exitCode == 0 {
		return *b.plan, nil
	}
	return roundhouse.Result{ExitCode: b.exitCode}, b.err
}

// Hosts satisfies the host lister the dispatcher probes for; a terraform run never uses it.
func (b *brokenPlanRunner) Hosts(context.Context, string, string) ([]string, error) {
	return nil, nil
}

// destroyGuardStore returns a policy store holding one plan-content rule with the given destroy
// limit, which is what puts a terraform apply through the plan gate at all.
func destroyGuardStore(t *testing.T, maxDestroy int) policy.Store {
	t.Helper()
	policies := policy.NewMemStore()
	if err := policies.Save(context.Background(), &policy.Policy{
		ID: policy.NewID(), Name: "tf-destroy-guard", Tool: run.ToolTerraform,
		MaxDestroy: maxDestroy,
	}); err != nil {
		t.Fatalf("policies.Save() error = %v", err)
	}
	return policies
}

// TestABrokenPlanProposesNoApply pins the gate's most consequential refusal. The gate exists so a
// destructive apply is weighed before it runs, and the weighing is done on the plan's own output. A
// plan that failed produced no verdict at all, so proposing an apply from it would queue a
// destruction nobody measured, on the strength of a run that did not work.
func TestABrokenPlanProposesNoApply(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says how the plan pass broke.
		Name string
		// ExitCode is what the plan exited with.
		ExitCode int
		// Err is what the runner returned.
		Err error
		// Output is whatever the plan managed to print first.
		Output string
	}{{ // Test 0: A nonzero plan exit, which is what an init or syntax failure looks like.
		Name: "a nonzero plan exit", ExitCode: 1,
		Output: "Error: Could not load plugin\n",
	}, { // Test 1: A plan that printed a perfectly readable summary and then still exited nonzero.
		Name: "a readable summary on a failing plan", ExitCode: 1,
		Output: "Plan: 0 to add, 0 to change, 0 to destroy.\nError: backend unavailable\n",
	}, { // Test 2: The tool could not be launched at all.
		Name: "the tool could not run", Err: errors.New("exec: terraform: not found"),
	}, { // Test 3: A launch failure that also managed to print a summary first.
		Name: "an error after a summary", Err: errors.New("signal: killed"),
		Output: "Plan: 0 to add, 0 to change, 9 to destroy.\n",
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			runner := &brokenPlanRunner{
				output: test.Output, exitCode: test.ExitCode, err: test.Err,
			}
			d := New(store, runner, nil, WithNoJanitor(),
				WithPolicies(destroyGuardStore(t, 3)), WithClaimInterval(time.Millisecond))
			defer d.Close()

			created, err := d.Submit(ctx, "", "",
				run.WithTool(run.ToolTerraform), run.WithCommand("infra/prod"))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			got := waitTerminal(t, store, created.ID)
			if got.Status != run.StatusFailed {
				t.Errorf("plan run status = %q, want failed: the plan did not work", got.Status)
			}

			// Give any proposal that was going to be made time to appear.
			time.Sleep(200 * time.Millisecond)
			if n := countProposals(t, store); n != 0 {
				t.Errorf("%d applies were proposed from a plan that failed, so a destruction "+
					"nobody could weigh is queued on the strength of a broken run", n)
			}
			if n := runner.applies.Load(); n != 0 {
				t.Errorf("%d applies executed off a failed plan", n)
			}
		})
	}
}

// TestAPlanWhoseRenderingCouldNotBeReadIsHeld pins the gate's fail-safe on the saved plan. The gate
// weighs a plan by its JSON rendering, and the runner leaves the rendering off when the tool would
// not render the plan or the rendering is larger than it holds. A plan nobody weighed is exactly
// what the hold is for, so the apply waits for a person rather than being queued as though it
// destroyed nothing, whatever the printed summary said.
func TestAPlanWhoseRenderingCouldNotBeReadIsHeld(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	runner := &brokenPlanRunner{
		output: "Plan: 0 to add, 0 to change, 0 to destroy.\n",
		plan:   &roundhouse.Result{ExitCode: 0, Drift: true, PlanFile: plantest.File},
	}
	d := New(store, runner, nil, WithNoJanitor(), WithPolicies(destroyGuardStore(t, 3)),
		WithClaimInterval(time.Millisecond))
	defer d.Close()

	created, err := d.Submit(ctx, "", "",
		run.WithTool(run.ToolTerraform), run.WithCommand("infra/prod"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if plan := waitTerminal(t, store, created.ID); plan.Status != run.StatusSucceeded {
		t.Fatalf("plan run status = %q, want succeeded: the plan itself worked", plan.Status)
	}
	proposal := waitProposal(t, store, created.ID)
	stored, err := store.Get(ctx, proposal.ID)
	if err != nil {
		t.Fatalf("Get(proposal) error = %v", err)
	}
	if stored.Status != run.StatusPendingApproval {
		t.Errorf("proposed apply status = %q, want pending_approval: a plan whose rendering could "+
			"not be read was never weighed", stored.Status)
	}
	if n := runner.applies.Load(); n != 0 {
		t.Errorf("%d applies executed off a plan nobody weighed", n)
	}
}

// TestPlanGateSkipsWhatItMustNotRegate walks the runs the gate deliberately lets past. Re-planning a
// proposed apply would loop forever, a dry run is already a plan, and a non-terraform tool has no
// plan to weigh. Each of these is a run that must reach the runner in a single pass.
func TestPlanGateSkipsWhatItMustNotRegate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says which run is being submitted.
		Name string
		// Run is the run handed to the gate.
		Run *run.Run
	}{{ // Test 0: An apply already proposed from a plan must never be planned again.
		Name: "a proposed apply",
		Run: &run.Run{Tool: run.ToolTerraform, Command: "infra/prod",
			ProposedFrom: "run_earlier_plan"},
	}, { // Test 1: A dry run is a plan already, so there is nothing to plan first.
		Name: "a dry run",
		Run:  &run.Run{Tool: run.ToolTerraform, Command: "infra/prod", DryRun: true},
	}, { // Test 2: A bash command has no plan to read, whatever the rule says.
		Name: "a different tool",
		Run:  &run.Run{Tool: run.ToolBash, Command: "echo hi"},
	}, { // Test 3: Ansible is likewise outside the plan gate.
		Name: "ansible",
		Run:  &run.Run{Tool: run.ToolAnsible, Playbook: "site.yml"},
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			d := New(run.NewMemStore(), okRunner(), nil, WithNoJanitor(),
				WithPolicies(destroyGuardStore(t, 3)))
			defer d.Close()

			policies, err := d.planGatePolicies(ctx, test.Run)
			if err != nil {
				t.Fatalf("planGatePolicies() error = %v", err)
			}
			if policies != nil {
				t.Errorf("the gate claimed this run, which would plan it before applying and, for a "+
					"proposed apply, loop forever: %+v", test.Run)
			}
		})
	}
}

// TestPlanGateClaimsATerraformApply is the positive half: with no gate the refusals above prove
// nothing, so the same rule must actually catch the run it was written for.
func TestPlanGateClaimsATerraformApply(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := New(run.NewMemStore(), okRunner(), nil, WithNoJanitor(),
		WithPolicies(destroyGuardStore(t, 3)))
	defer d.Close()

	policies, err := d.planGatePolicies(ctx,
		&run.Run{Tool: run.ToolTerraform, Command: "infra/prod"})
	if err != nil {
		t.Fatalf("planGatePolicies() error = %v", err)
	}
	if len(policies) == 0 {
		t.Error("a terraform apply under a plan-content rule was not gated, so every refusal above " +
			"would pass on a gate that never fires")
	}
}

// TestPlanGateWithNoPoliciesConfiguredGatesNothing pins the install with the feature off. Reading a
// nil policy store would panic on the start path of every terraform run, so the guard has to come
// first.
func TestPlanGateWithNoPoliciesConfiguredGatesNothing(t *testing.T) {
	t.Parallel()
	d := New(run.NewMemStore(), okRunner(), nil, WithNoJanitor())
	defer d.Close()

	policies, err := d.planGatePolicies(context.Background(),
		&run.Run{Tool: run.ToolTerraform, Command: "infra/prod"})
	if err != nil {
		t.Fatalf("planGatePolicies() error = %v", err)
	}
	if policies != nil {
		t.Errorf("policies = %v, want nil when the feature is off", policies)
	}
}

// TestTheGateWeighsTheSavedPlanNotThePrintedSummary pins where the destroy count comes from. The
// apply carries out the saved plan file, so the count the limit is held against has to be the one in
// that file, read from its JSON rendering, never the text the tool printed: a summary that reads as
// destroying nothing over a plan file that destroys nine must not queue an apply that destroys nine.
func TestTheGateWeighsTheSavedPlanNotThePrintedSummary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	runner := &brokenPlanRunner{
		output: "Plan: 0 to add, 0 to change, 0 to destroy.\n",
		plan: &roundhouse.Result{ExitCode: 0, Drift: true, PlanFile: plantest.File,
			PlanJSON: plantest.JSON(9)},
	}
	d := New(store, runner, nil, WithNoJanitor(), WithPolicies(destroyGuardStore(t, 3)),
		WithClaimInterval(time.Millisecond))
	defer d.Close()

	created, err := d.Submit(ctx, "", "",
		run.WithTool(run.ToolTerraform), run.WithCommand("infra/prod"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if plan := waitTerminal(t, store, created.ID); plan.Status != run.StatusSucceeded {
		t.Fatalf("plan run status = %q, want succeeded: the plan itself worked", plan.Status)
	}
	proposal := waitProposal(t, store, created.ID)
	stored, err := store.Get(ctx, proposal.ID)
	if err != nil {
		t.Fatalf("Get(proposal) error = %v", err)
	}
	if stored.Status != run.StatusPendingApproval {
		t.Errorf("proposed apply status = %q, want pending_approval: the saved plan destroys nine "+
			"against a limit of three", stored.Status)
	}
	if stored.PlanDestroys == nil || *stored.PlanDestroys != 9 {
		t.Errorf("recorded destroys = %v, want the saved plan's nine", stored.PlanDestroys)
	}
}
