package dispatch

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// destroyPolicy returns a store holding one blanket policy that gates terraform destroy, the shape
// an operator writes to keep a destructive command behind an approver.
func destroyPolicy(t *testing.T) policy.Store {
	t.Helper()
	policies := policy.NewMemStore()
	if err := policies.Save(context.Background(), &policy.Policy{
		ID: policy.NewID(), Name: "tf-destroy", Tool: "terraform", CommandContains: "destroy",
		MaxDestroy: policy.DisabledMaxDestroy,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	return policies
}

// countingRunner returns a runner that succeeds and records how many times it was asked to execute,
// so a test can prove a held pipeline ran nothing at all.
func countingRunner(executions *int) roundhouse.Runner {
	return roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			*executions++
			return roundhouse.Result{ExitCode: 0}, nil
		},
	)
}

// TestPolicyHoldsPipelineWithMatchingStep confirms an approval policy cannot be skipped by wrapping
// the gated command in a workflow. A pipeline is submitted through a different path than a single
// run, so without this the same terraform destroy that is held on its own executes freely as a
// one-step workflow.
func TestPolicyHoldsPipelineWithMatchingStep(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	executions := 0
	d := New(store, countingRunner(&executions), nil, WithPolicies(destroyPolicy(t)))
	defer d.Close()

	steps := []run.PipelineStep{
		{Name: "plan", Tool: "terraform", Command: "terraform plan"},
		{Name: "wreck", Tool: "terraform", Command: "terraform destroy prod"},
	}
	parent, err := d.SubmitPipeline(context.Background(), "release", "hosts.ini", steps)
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	if parent.Status != run.StatusPendingApproval {
		t.Fatalf("pipeline status = %q, want pending_approval since a step matches a policy",
			parent.Status)
	}

	// Nothing may execute while the pipeline waits, including the steps that match no policy.
	time.Sleep(100 * time.Millisecond)
	got, err := store.Get(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusPendingApproval {
		t.Errorf("held pipeline status = %q, want still pending_approval", got.Status)
	}
	children, err := store.Steps(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Steps() error = %v", err)
	}
	if len(children) != 0 {
		t.Errorf("held pipeline started %d steps, want 0", len(children))
	}
	if executions != 0 {
		t.Errorf("held pipeline executed %d commands, want 0", executions)
	}
}

// TestApprovedPipelineRunsItsSteps confirms approving a held pipeline actually runs it. A gate that
// holds correctly but never releases would strand every gated workflow, so the release path is
// asserted rather than assumed.
func TestApprovedPipelineRunsItsSteps(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	executions := 0
	d := New(store, countingRunner(&executions), nil, WithPolicies(destroyPolicy(t)))
	defer d.Close()

	steps := []run.PipelineStep{
		{Name: "wreck", Tool: "terraform", Command: "terraform destroy prod"},
	}
	parent, err := d.SubmitPipeline(context.Background(), "teardown", "hosts.ini", steps)
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	if parent.Status != run.StatusPendingApproval {
		t.Fatalf("pipeline status = %q, want pending_approval", parent.Status)
	}

	if _, err := d.Approve(context.Background(), parent.ID); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := store.Get(context.Background(), parent.ID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if got.Status == run.StatusSucceeded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("approved pipeline status = %q, want succeeded", got.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	children, err := store.Steps(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Steps() error = %v", err)
	}
	if len(children) != 1 {
		t.Errorf("approved pipeline ran %d steps, want 1", len(children))
	}
}

// TestRejectedPipelineNeverRuns confirms rejecting a held pipeline is terminal, so a denied workflow
// cannot execute later.
func TestRejectedPipelineNeverRuns(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	executions := 0
	d := New(store, countingRunner(&executions), nil, WithPolicies(destroyPolicy(t)))
	defer d.Close()

	steps := []run.PipelineStep{
		{Name: "wreck", Tool: "terraform", Command: "terraform destroy prod"},
	}
	parent, err := d.SubmitPipeline(context.Background(), "teardown", "hosts.ini", steps)
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	if _, err := d.Reject(context.Background(), parent.ID, "not today"); err != nil {
		t.Fatalf("Reject() error = %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	got, err := store.Get(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusRejected {
		t.Errorf("rejected pipeline status = %q, want rejected", got.Status)
	}
	if executions != 0 {
		t.Errorf("rejected pipeline executed %d commands, want 0", executions)
	}
}

// TestPipelineRequireApprovalOptIn confirms a workflow can be held on request even where no policy
// matches, matching what a single run already allowed.
func TestPipelineRequireApprovalOptIn(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	executions := 0
	d := New(store, countingRunner(&executions), nil)
	defer d.Close()

	steps := []run.PipelineStep{{Name: "deploy", Tool: "bash", Command: "deploy.sh"}}
	parent, err := d.SubmitPipeline(context.Background(), "release", "hosts.ini", steps,
		run.WithRequireApproval(true))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	if parent.Status != run.StatusPendingApproval {
		t.Fatalf("pipeline status = %q, want pending_approval", parent.Status)
	}
	time.Sleep(100 * time.Millisecond)
	if executions != 0 {
		t.Errorf("held pipeline executed %d commands, want 0", executions)
	}
}

// TestPipelineWithoutMatchingStepRuns confirms the gate narrows to workflows that actually contain a
// gated step, so ordinary workflows are unaffected.
func TestPipelineWithoutMatchingStepRuns(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	executions := 0
	d := New(store, countingRunner(&executions), nil, WithPolicies(destroyPolicy(t)))
	defer d.Close()

	steps := []run.PipelineStep{
		{Name: "plan", Tool: "terraform", Command: "terraform plan"},
		{Name: "apply", Tool: "terraform", Command: "terraform apply"},
	}
	parent, err := d.SubmitPipeline(context.Background(), "release", "hosts.ini", steps)
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	if parent.Status == run.StatusPendingApproval {
		t.Fatal("pipeline with no gated step was held for approval")
	}
}
