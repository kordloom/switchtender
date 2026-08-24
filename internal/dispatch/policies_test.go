package dispatch

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestPolicyHoldsMatchingRun confirms a run matching a stored policy is held for approval at submit
// without any opt-in, while a non-matching run runs normally.
func TestPolicyHoldsMatchingRun(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	policies := policy.NewMemStore()
	if err := policies.Save(context.Background(), &policy.Policy{
		ID: policy.NewID(), Name: "tf-destroy", Tool: "terraform", CommandContains: "destroy",
		MaxDestroy: policy.DisabledMaxDestroy,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		},
	)
	d := New(store, runner, nil, WithPolicies(policies))
	defer d.Close()

	// A matching run is held for approval with no opt-in.
	held, err := d.Submit(context.Background(), "", "",
		run.WithTool("terraform"), run.WithCommand("terraform destroy prod"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if held.Status != run.StatusPendingApproval {
		t.Fatalf("matching run status = %q, want pending_approval", held.Status)
	}
	time.Sleep(50 * time.Millisecond)
	got, err := store.Get(context.Background(), held.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusPendingApproval {
		t.Errorf("held run status = %q, want still pending_approval", got.Status)
	}

	// A non-matching run is not held and runs to completion.
	free, err := d.Submit(context.Background(), "", "",
		run.WithTool("terraform"), run.WithCommand("terraform apply"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if free.Status == run.StatusPendingApproval {
		t.Fatal("non-matching run should not be held")
	}
	final := waitTerminal(t, store, free.ID)
	if final.Status != run.StatusSucceeded {
		t.Errorf("non-matching run status = %q, want succeeded", final.Status)
	}
}

// planRunner returns a runner that emits destroyLine for a plan (dry run) and a plain apply line for a
// real run, so a plan-content gate test controls the destroy count its plan reports.
func planRunner(destroyLine string) roundhouse.Runner {
	return roundhouse.RunnerFunc(
		func(_ context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
			if spec.DryRun {
				_, _ = io.WriteString(out, destroyLine+"\n")
				return roundhouse.Result{ExitCode: 0, Drift: true}, nil
			}
			_, _ = io.WriteString(out, "Apply complete!\n")
			return roundhouse.Result{ExitCode: 0}, nil
		})
}

// waitProposal polls the store until a run proposed from parentID appears, or the deadline passes.
func waitProposal(t *testing.T, store run.Store, parentID string) *run.Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		runs, err := store.List(context.Background())
		if err == nil {
			for _, r := range runs {
				if r.ProposedFrom == parentID {
					return r
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no apply proposed from %s", parentID)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// countProposals returns how many stored runs were proposed from another run.
func countProposals(t *testing.T, store run.Store) int {
	t.Helper()
	runs, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	n := 0
	for _, r := range runs {
		if r.ProposedFrom != "" {
			n++
		}
	}
	return n
}

// TestPlanGateHeldWhenPlanDestroysTooMuch confirms a terraform apply matching a plan-content policy
// runs a plan first and, when the plan destroys more than the threshold, proposes an apply held for
// approval while the plan run itself succeeds.
func TestPlanGateHeldWhenPlanDestroysTooMuch(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	policies := policy.NewMemStore()
	if err := policies.Save(context.Background(), &policy.Policy{
		ID: policy.NewID(), Name: "tf-destroy-guard", Tool: run.ToolTerraform, MaxDestroy: 1,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	d := New(store, planRunner("Plan: 0 to add, 0 to change, 3 to destroy"), nil, WithPolicies(policies))
	defer d.Close()

	created, err := d.Submit(context.Background(), "", "",
		run.WithTool(run.ToolTerraform), run.WithCommand("infra/prod"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if created.Status == run.StatusPendingApproval {
		t.Fatal("a plan-content policy must not blanket-hold the apply at submit")
	}

	// The plan run itself succeeds: it ran a plan and proposed an apply rather than applying.
	plan := waitTerminal(t, store, created.ID)
	if plan.Status != run.StatusSucceeded {
		t.Fatalf("plan run status = %q, want succeeded", plan.Status)
	}

	proposal := waitProposal(t, store, created.ID)
	if proposal.Status != run.StatusPendingApproval {
		t.Errorf("proposed apply status = %q, want pending_approval (held)", proposal.Status)
	}
	if proposal.DryRun {
		t.Error("proposed apply should be a real apply, not a dry run")
	}
	if proposal.Command != "infra/prod" || run.NormalizeTool(proposal.Tool) != run.ToolTerraform {
		t.Errorf("proposal = %+v, want a terraform apply of infra/prod", proposal)
	}
	if n := countProposals(t, store); n != 1 {
		t.Errorf("proposal count = %d, want exactly 1", n)
	}
}

// TestPlanGateQueuesApplyWithinThreshold confirms a terraform apply whose plan destroys at or under
// the threshold proposes an apply that runs to completion without approval, and that the proposed
// apply does not itself re-gate into another plan.
func TestPlanGateQueuesApplyWithinThreshold(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	policies := policy.NewMemStore()
	if err := policies.Save(context.Background(), &policy.Policy{
		ID: policy.NewID(), Name: "tf-destroy-guard", Tool: run.ToolTerraform, MaxDestroy: 5,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	d := New(store, planRunner("Plan: 1 to add, 0 to change, 2 to destroy"), nil, WithPolicies(policies))
	defer d.Close()

	created, err := d.Submit(context.Background(), "", "",
		run.WithTool(run.ToolTerraform), run.WithCommand("infra/dev"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if plan := waitTerminal(t, store, created.ID); plan.Status != run.StatusSucceeded {
		t.Fatalf("plan run status = %q, want succeeded", plan.Status)
	}

	proposal := waitProposal(t, store, created.ID)
	applied := waitTerminal(t, store, proposal.ID)
	if applied.Status != run.StatusSucceeded {
		t.Errorf("proposed apply status = %q, want succeeded (applied)", applied.Status)
	}
	if applied.ProposedFrom != created.ID || applied.DryRun {
		t.Errorf("proposal = %+v, want a real apply proposed from %s", applied, created.ID)
	}
	// The proposed apply carries ProposedFrom, so it must not re-gate: exactly one proposal exists.
	if n := countProposals(t, store); n != 1 {
		t.Errorf("proposal count = %d, want exactly 1 (no re-gate loop)", n)
	}
}

// TestPlanGateLeavesUngatedRuns confirms the plan gate ignores a dry run and a non-terraform run even
// under a matching plan-content policy: both run in a single phase and propose nothing.
func TestPlanGateLeavesUngatedRuns(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	policies := policy.NewMemStore()
	if err := policies.Save(context.Background(), &policy.Policy{
		ID: policy.NewID(), Name: "tf-destroy-guard", Tool: run.ToolTerraform, MaxDestroy: 0,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	d := New(store, planRunner("Plan: 0 to add, 0 to change, 9 to destroy"), nil, WithPolicies(policies))
	defer d.Close()

	// A dry run is a preview and is never plan-gated.
	dry, err := d.Submit(context.Background(), "", "",
		run.WithTool(run.ToolTerraform), run.WithCommand("infra"), run.WithDryRun(true))
	if err != nil {
		t.Fatalf("Submit() dry run error = %v", err)
	}
	if done := waitTerminal(t, store, dry.ID); done.Status != run.StatusSucceeded {
		t.Errorf("dry run status = %q, want succeeded", done.Status)
	}

	// A non-terraform run is out of the plan gate's scope entirely.
	bash, err := d.Submit(context.Background(), "", "",
		run.WithTool(run.ToolBash), run.WithCommand("echo hi"))
	if err != nil {
		t.Fatalf("Submit() bash error = %v", err)
	}
	if done := waitTerminal(t, store, bash.ID); done.Status != run.StatusSucceeded {
		t.Errorf("bash run status = %q, want succeeded", done.Status)
	}

	if n := countProposals(t, store); n != 0 {
		t.Errorf("ungated runs proposed %d applies, want 0", n)
	}
}

// TestDenyPolicyRefusesSubmission proves a deny policy refuses the submission outright: the run is
// never created, the refusal names the rule, a run born held is refused too rather than parked in
// front of an approver, and wrapping the refused command in a pipeline step does not launder it.
func TestDenyPolicyRefusesSubmission(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	policies := policy.NewMemStore()
	if err := policies.Save(context.Background(), &policy.Policy{
		ID: policy.NewID(), Name: "no agent drops", ActorKind: policy.ActorKindAgent,
		CommandContains: "drop database", Effect: policy.EffectDeny,
		MaxDestroy: policy.DisabledMaxDestroy,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		},
	)
	d := New(store, runner, nil, WithPolicies(policies))
	defer d.Close()

	deniedOpts := []run.SubmitOption{
		run.WithTool("bash"), run.WithCommand("psql -c 'drop database prod'"),
		run.WithActor("prod-remediator"), run.WithActorType("agent"),
	}
	if _, err := d.Submit(context.Background(), "", "", deniedOpts...); !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("Submit() error = %v, want ErrPolicyDenied", err)
	} else if !strings.Contains(err.Error(), "no agent drops") {
		t.Errorf("refusal %q does not name the rule", err)
	}

	// Asking for approval at submission must not soften a deny into a hold.
	heldOpts := append(deniedOpts, run.WithRequireApproval(true))
	if _, err := d.Submit(context.Background(), "", "", heldOpts...); !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("Submit(born held) error = %v, want ErrPolicyDenied", err)
	}

	// The same command from a person is outside the rule's scope and proceeds.
	if _, err := d.Submit(context.Background(), "", "",
		run.WithTool("bash"), run.WithCommand("psql -c 'drop database prod'"),
		run.WithActor("dba-jane"), run.WithActorType("session")); err != nil {
		t.Fatalf("Submit(human) error = %v, want nil", err)
	}

	// A pipeline whose step matches is refused whole.
	steps := []run.PipelineStep{{Name: "drop", Tool: "bash", Command: "psql -c 'drop database prod'"}}
	if _, err := d.SubmitPipeline(context.Background(), "wrap", "", steps,
		run.WithActor("prod-remediator"), run.WithActorType("agent")); !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("SubmitPipeline() error = %v, want ErrPolicyDenied", err)
	}

	// Nothing denied was created.
	runs, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, r := range runs {
		if strings.Contains(r.Command, "drop database") && r.ActorType == "agent" {
			t.Errorf("a denied submission left run %s in the store", r.ID)
		}
	}
}

// TestPlanGateCarriesDistinctApproverOntoTheApply covers the one rule the dispatcher's policy pass
// cannot reach.
//
// policy.Requiring only considers rules with no destroy limit, so a plan-content rule is excluded
// from it by design. The plan gate finds that rule itself with policy.Exceeding and held the apply
// correctly, naming the rule and the count, but it copied only the name. The requirement went
// nowhere, so the run with the largest blast radius in the product, an apply that blew past its
// destroy limit, was the one an operator could release for themselves.
func TestPlanGateCarriesDistinctApproverOntoTheApply(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	policies := policy.NewMemStore()
	if err := policies.Save(ctx, &policy.Policy{
		ID: policy.NewID(), Name: "prod destroy limit", Tool: run.ToolTerraform, MaxDestroy: 2,
		RequireDistinctApprover: true,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	d := New(store, planRunner("Plan: 0 to add, 0 to change, 9 to destroy"), nil,
		WithPolicies(policies))
	defer d.Close()

	created, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform),
		run.WithCommand("infra/prod"), run.WithActor("casey"), run.WithActorType("session"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitTerminal(t, store, created.ID)

	proposal := waitProposal(t, store, created.ID)
	if proposal.Status != run.StatusPendingApproval {
		t.Fatalf("proposed apply status = %q, want pending_approval", proposal.Status)
	}
	if !proposal.RequireDistinctApprover {
		t.Errorf("the apply held for destroying 9 against a limit of 2 does not carry its rule's "+
			"distinct-approver requirement, so %q can release their own destroy", proposal.Actor)
	}
	if !strings.Contains(proposal.HeldByPolicy, "prod destroy limit") {
		t.Errorf("held by = %q, want it to name the rule", proposal.HeldByPolicy)
	}
	if _, err := d.Approve(ctx, proposal.ID, "casey", "session"); !errors.Is(err, ErrSelfApproval) {
		t.Errorf("self approval error = %v, want ErrSelfApproval", err)
	}
	if _, err := d.Approve(ctx, proposal.ID, "dana", "session"); err != nil {
		t.Errorf("a second person could not approve the apply: %v", err)
	}
}
