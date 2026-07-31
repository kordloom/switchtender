package dispatch

import (
	"context"
	"io"
	"sync/atomic"
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

// countingRunnerLister succeeds at everything, lists a fixed host set so a split can shard, and
// records how many times it was asked to execute, so a test can prove a held split ran nothing.
// The count is atomic because the shards of a split execute concurrently, unlike pipeline steps.
type countingRunnerLister struct {
	// executions counts calls to Run.
	executions atomic.Int64
	// hosts is returned by Hosts.
	hosts []string
}

// Run reports success and counts the execution.
func (c *countingRunnerLister) Run(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
	c.executions.Add(1)
	return roundhouse.Result{ExitCode: 0}, nil
}

// Hosts returns the fixed host set.
func (c *countingRunnerLister) Hosts(context.Context, string) ([]string, error) {
	return c.hosts, nil
}

// waitForStatus blocks until the run reaches want or the deadline passes, failing the test with the
// status it was actually left in.
func waitForStatus(t *testing.T, store run.Store, id string, want run.Status) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last run.Status
	for time.Now().Before(deadline) {
		got, err := store.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		last = got.Status
		if last == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s status = %q after 5s, want %q", id, last, want)
}

// ansibleWidePolicy gates every Ansible run, the blanket shape an operator writes to keep changes
// behind an approver. Splitting only ever happens for Ansible, so this is the policy a split has to
// respect.
func ansibleWidePolicy(t *testing.T) policy.Store {
	t.Helper()
	policies := policy.NewMemStore()
	if err := policies.Save(context.Background(), &policy.Policy{
		ID: policy.NewID(), Name: "all-ansible", Tool: "ansible",
		MaxDestroy: policy.DisabledMaxDestroy,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	return policies
}

// TestPolicyHoldsSplit confirms an approval policy cannot be skipped by sharding the run.
//
// A split is submitted through a different path than a single run and that path never consulted the
// policy store, so the identical playbook that Submit held for an approver executed on every host
// the moment it was split in two. The shards ran and finished; nothing was ever held. This is the
// same bypass that was closed for pipelines, in the one remaining path that has its own submit.
func TestPolicyHoldsSplit(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	runner := &countingRunnerLister{hosts: []string{"web01", "web02", "web03", "web04"}}
	d := New(store, runner, nil, WithPolicies(ansibleWidePolicy(t)))
	defer d.Close()
	ctx := context.Background()

	parent, err := d.SubmitSplit(ctx, "site.yml", "inv", 2)
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	if parent.Status != run.StatusPendingApproval {
		t.Fatalf("split parent status = %q, want pending_approval: the same run is held when it "+
			"is submitted unsharded", parent.Status)
	}

	// Give a coordinator that should not exist every chance to start the shards anyway.
	time.Sleep(300 * time.Millisecond)
	got, err := store.Get(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusPendingApproval {
		t.Errorf("stored parent status = %q, want pending_approval: a coordinator started a held "+
			"split", got.Status)
	}
	shards, err := store.Shards(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	if len(shards) == 0 {
		t.Fatal("a held split stored no shards, so approving it after a restart cannot run it")
	}
	for _, s := range shards {
		if s.Status != run.StatusPendingApproval {
			t.Errorf("shard %s status = %q, want pending_approval: a pending shard is claimable by "+
				"any worker, which executes the run the policy gated", s.ID, s.Status)
		}
	}
	if got := runner.executions.Load(); got != 0 {
		t.Errorf("the gated playbook executed %d times while awaiting approval", got)
	}
}

// TestApproveStartsHeldSplit confirms an approved split actually runs, since no claim loop picks up
// a parent and a split that never starts is as broken as one that skips the gate.
func TestApproveStartsHeldSplit(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	runner := &countingRunnerLister{hosts: []string{"web01", "web02", "web03", "web04"}}
	d := New(store, runner, nil, WithPolicies(ansibleWidePolicy(t)))
	defer d.Close()
	ctx := context.Background()

	parent, err := d.SubmitSplit(ctx, "site.yml", "inv", 2)
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	if _, err := d.Approve(ctx, parent.ID); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	waitForStatus(t, store, parent.ID, run.StatusSucceeded)
	if runner.executions.Load() == 0 {
		t.Error("an approved split executed nothing")
	}
}

// TestRejectSettlesHeldSplitShards confirms rejecting a split settles the shards stored with it, so
// none is left awaiting a decision that has already been made.
func TestRejectSettlesHeldSplitShards(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	runner := &countingRunnerLister{hosts: []string{"web01", "web02", "web03", "web04"}}
	d := New(store, runner, nil, WithPolicies(ansibleWidePolicy(t)))
	defer d.Close()
	ctx := context.Background()

	parent, err := d.SubmitSplit(ctx, "site.yml", "inv", 2)
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	if _, err := d.Reject(ctx, parent.ID, "not this week"); err != nil {
		t.Fatalf("Reject() error = %v", err)
	}
	shards, err := store.Shards(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	if len(shards) == 0 {
		t.Fatal("no shards were stored")
	}
	for _, s := range shards {
		if !s.Status.Terminal() {
			t.Errorf("shard %s status = %q after the split was rejected, want a terminal state",
				s.ID, s.Status)
		}
	}
	if got := runner.executions.Load(); got != 0 {
		t.Errorf("the rejected playbook executed %d times", got)
	}
}
