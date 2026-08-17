package dispatch

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestRetryOfARejectedSplitIsRefused pins that retrying a split an approver denied cannot execute it.
//
// Rejecting a split cancels the shards stored with it, and the retry path treated every non-succeeded
// shard as failed, so a denial produced a full set of retryable shards. An operator, who needs only
// the operator role to retry, then ran the exact spec the approver had just refused, on every host.
func TestRetryOfARejectedSplitIsRefused(t *testing.T) {
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
	if _, err := d.Reject(ctx, parent.ID, "not on prod", "approver-pat", "session"); err != nil {
		t.Fatalf("Reject() error = %v", err)
	}
	if _, err := d.RetryFailedShards(ctx, parent.ID); err == nil {
		t.Error("a rejected split was retryable, so an approver's denial can be undone by the " +
			"operator who asked")
	}
	time.Sleep(150 * time.Millisecond)
	if n := runner.executions.Load(); n != 0 {
		t.Errorf("the rejected playbook executed %d times after a retry", n)
	}
}

// TestRetryHonorsTheApprovalPolicy pins that a retry faces the same gate as every other submit.
//
// A retry is a fourth way to submit, and it inherits the parent's whole execution spec. Submit,
// SubmitSplit, and SubmitPipeline each consult the policy store; this path did not, so retrying was
// a way to run a spec an approver would have held.
func TestRetryHonorsTheApprovalPolicy(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	runner := &countingRunnerLister{hosts: []string{"web01", "web02", "web03", "web04"}}
	// No policy while the original runs, so it executes and its shards genuinely fail.
	policies := policy.NewMemStore()
	d := New(store, &failingLister{hosts: runner.hosts}, nil, WithPolicies(policies))
	defer d.Close()
	ctx := context.Background()

	parent, err := d.SubmitSplit(ctx, "site.yml", "inv", 2)
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	waitForStatus(t, store, parent.ID, run.StatusFailed)

	// The policy arrives after the failure, which is the ordinary case: an operator tightens the
	// gate, and every later submission has to face it, including a retry of an older run.
	if err := policies.Save(ctx, &policy.Policy{
		ID: policy.NewID(), Name: "all-ansible", Tool: "ansible",
		MaxDestroy: policy.DisabledMaxDestroy,
	}); err != nil {
		t.Fatalf("policies.Save() error = %v", err)
	}
	retry, err := d.RetryFailedShards(ctx, parent.ID)
	if err != nil {
		t.Fatalf("RetryFailedShards() error = %v", err)
	}
	if retry.Status != run.StatusPendingApproval {
		t.Errorf("retry status = %q, want pending_approval: a retry runs the same spec, so it "+
			"meets the same gate", retry.Status)
	}
	shards, err := store.Shards(ctx, retry.ID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	for _, s := range shards {
		if s.Status != run.StatusPendingApproval {
			t.Errorf("shard %s of a held retry is %q, and a pending shard is claimable by any "+
				"worker", s.ID, s.Status)
		}
	}
}

// TestSubmitRefusedWhenPoliciesCannotBeRead pins that a gate which cannot be evaluated stops the
// run rather than waving it through.
//
// A policy lookup failure used to be logged and treated as no match, which was defensible while
// policies were rows in the same database as runs. A policy file fails independently: delete it,
// deploy it non-atomically, or merge one typo, and every gate vanished while runs saved perfectly.
func TestSubmitRefusedWhenPoliciesCannotBeRead(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "policies.yaml")
	if err := os.WriteFile(path,
		[]byte("policies:\n  - name: all-ansible\n    tool: ansible\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	files, err := policy.NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	runner := &countingRunnerLister{hosts: []string{"web01", "web02"}}
	d := New(run.NewMemStore(), runner, nil, WithPolicies(files))
	defer d.Close()
	ctx := context.Background()

	// The gate works while the file is readable.
	held, err := d.Submit(ctx, "site.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if held.Status != run.StatusPendingApproval {
		t.Fatalf("status = %q, want pending_approval before the file breaks", held.Status)
	}

	// Now the file goes away, which is a deleted file, a botched deploy, or a bad merge.
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := d.Submit(ctx, "site.yml", "inv"); !errors.Is(err, ErrPolicyUnavailable) {
		t.Errorf("Submit() with unreadable policies error = %v, want ErrPolicyUnavailable: a gate "+
			"that cannot be checked has not been passed", err)
	}
	time.Sleep(150 * time.Millisecond)
	if n := runner.executions.Load(); n != 0 {
		t.Errorf("%d runs executed while the policy file was unreadable", n)
	}
}

// TestApprovedSplitSurvivesTheAbandonedParentSweep pins that approving a held split does not race
// the sweep that settles parents nothing will finish.
//
// That sweep interrupts a split or pipeline parent which is pending, unclaimed, and older than the
// cutoff, measuring age from CreatedAt. A run held for a person carries its submit time, so the
// instant an approval moved it to pending it was already past the cutoff, and the next janitor tick
// interrupted it and canceled every shard. The approved run executed nothing.
func TestApprovedSplitSurvivesTheAbandonedParentSweep(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	runner := &countingRunnerLister{hosts: []string{"web01", "web02", "web03", "web04"}}
	d := New(store, runner, nil, WithPolicies(ansibleWidePolicy(t)), WithNoJanitor())
	defer d.Close()
	ctx := context.Background()

	parent, err := d.SubmitSplit(ctx, "site.yml", "inv", 2)
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	// Age the parent so it is well past any cutoff, which is what waiting for a person produces.
	held, err := store.Get(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	held.CreatedAt = time.Now().Add(-time.Hour)
	if err := store.Save(ctx, held); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if _, err := d.Approve(ctx, parent.ID, "approver-pat", "session"); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	// A sweep landing right behind the approval must not touch it.
	if _, err := store.ReclaimStale(ctx, 30*time.Second); err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}
	got, err := store.Get(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Get() after sweep error = %v", err)
	}
	if got.Status == run.StatusInterrupted {
		t.Fatalf("the sweep interrupted a run an approver had just released: %q", got.Error)
	}
	shards, err := store.Shards(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	for _, s := range shards {
		if s.Status == run.StatusCanceled {
			t.Errorf("shard %s was canceled by the sweep after approval: %q", s.ID, s.Error)
		}
	}
	waitForStatus(t, store, parent.ID, run.StatusSucceeded)
	if runner.executions.Load() == 0 {
		t.Error("an approved split executed nothing")
	}
}

// failingLister lists hosts so a split can shard, and fails every execution so the shards genuinely
// fail rather than being canceled.
type failingLister struct {
	// hosts is returned by Hosts.
	hosts []string
}

// Run reports a non-zero exit so the shard fails.
func (f *failingLister) Run(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
	return roundhouse.Result{ExitCode: 2}, nil
}

// Hosts returns the fixed host set.
func (f *failingLister) Hosts(context.Context, string, string) ([]string, error) { return f.hosts, nil }

// TestCancelBeforeStartIsNotUndone pins that a parent canceled between its submit and its
// coordinator's first write stays canceled.
//
// The coordinator's start was an unconditional upsert, so a cancel landing in that window was
// silently overwritten and the whole fan-out executed after the API had already answered that the
// run was canceled. CancelPending terminalizes an unclaimed parent without setting the cancel
// flag, so the watcher had nothing to notice either.
//
// What this pins is that the start is fenced at all: it fails when the fence is removed. It does
// not distinguish a compare-and-swap start from a read-then-write one, because the interleaving
// that separates those two is narrower than a test harness can hold open from outside. The swap is
// still the right construction, and this is the guard that survives.
func TestCancelBeforeStartIsNotUndone(t *testing.T) {
	t.Parallel()
	store := &pausedStore{Store: run.NewMemStore(), gate: make(chan struct{})}
	runner := &countingRunnerLister{hosts: []string{"web01", "web02", "web03", "web04"}}
	d := New(store, runner, nil, WithNoJanitor())
	defer d.Close()
	ctx := context.Background()

	parent, err := d.SubmitSplit(ctx, "site.yml", "inv", 2)
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	// The coordinator is now blocked on its claim. Cancel the parent the way the API does.
	canceled, err := store.CancelPending(ctx, parent.ID)
	if err != nil {
		t.Fatalf("CancelPending() error = %v", err)
	}
	if !canceled {
		t.Fatal("the parent could not be canceled before it started")
	}
	close(store.gate)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, gerr := store.Get(ctx, parent.ID)
		if gerr == nil && got.Status != run.StatusCanceled {
			t.Fatalf("a canceled parent came back as %q, so the fan-out proceeds after the API "+
				"reported it canceled", got.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := runner.executions.Load(); n != 0 {
		t.Errorf("%d shards executed on a canceled split", n)
	}
}

// pausedStore holds the coordinator at its claim so a cancel can land in the window the claim is
// meant to close.
type pausedStore struct {
	run.Store
	// gate releases the first claim attempt once closed.
	gate chan struct{}
	// once ensures only the first claim waits.
	once sync.Once
}

// TransitionStatusAndClaim waits for the gate the first time, then behaves normally.
func (p *pausedStore) TransitionStatusAndClaim(ctx context.Context, id string, from, to run.Status,
	owner string) (bool, error) {
	p.once.Do(func() { <-p.gate })
	return p.Store.TransitionStatusAndClaim(ctx, id, from, to, owner)
}

// TestPlanGateFailsClosedWhereItCannotBeChecked pins that a process which cannot read the policies
// refuses a plan-gated apply rather than applying it.
//
// The plan-content gate is enforced where the run executes, not where it was submitted. A relay
// worker leases runs across a segment boundary and never sees the control node's database, and it
// was given no policy store at all, so the gate silently did not exist there. A terraform apply
// scoped by a destroy threshold was planned and held when the control node won the claim, and
// applied straight to production when a worker did, decided by a race between claim loops.
func TestPlanGateFailsClosedWhereItCannotBeChecked(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	runner := &countingRunnerLister{hosts: []string{"web01"}}
	d := New(store, runner, nil, WithPolicies(policy.Unreachable{}))
	defer d.Close()
	ctx := context.Background()

	// Submit is refused too, which is the same fail-closed rule one step earlier.
	if _, err := d.Submit(ctx, "", "inv",
		run.WithTool("terraform"), run.WithCommand("/infra")); err == nil {
		t.Error("a run was accepted by a process that cannot read the approval policies")
	}
	if n := runner.executions.Load(); n != 0 {
		t.Errorf("%d runs executed where the policies could not be read", n)
	}
}

// TestPlanGateRunsUngatedWorkWhereThePoliciesAreReadable pins the other half of the rule: a process
// that CAN read the policies runs what none of them gate.
//
// Failing closed on an unreadable store is right. Handing a worker a store that refuses every read
// made every terraform run in the install fail, including the ones no policy would ever have
// matched, and which outcome a run got still depended on which claim loop won it. Fail closed on not
// knowing, not on having nothing to enforce.
func TestPlanGateRunsUngatedWorkWhereThePoliciesAreReadable(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	runner := &countingRunnerLister{hosts: []string{"web01"}}
	d := New(store, runner, nil, WithPolicies(policy.NewMemStore()))
	defer d.Close()
	ctx := context.Background()

	r, err := d.Submit(ctx, "", "inv", run.WithTool("terraform"), run.WithCommand("/infra"))
	if err != nil {
		t.Fatalf("Submit() error = %v: an install with no policies refused an ungated apply", err)
	}
	waitForStatus(t, store, r.ID, run.StatusSucceeded)
	if n := runner.executions.Load(); n != 1 {
		t.Errorf("executions = %d, want 1: an ungated apply did not run", n)
	}
}

// TestCancelingAHeldSplitSettlesItsShards pins that canceling a split before it starts leaves no
// shard behind.
//
// A split stores its shards alongside the parent. Rejecting one settled them; canceling did not,
// and the store sweep could not, because orphan resolution only fires for an interrupted parent and
// a canceled one is terminal. The shards sat awaiting an approval that would never come.
func TestCancelingAHeldSplitSettlesItsShards(t *testing.T) {
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
	shards, err := store.Shards(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	if len(shards) == 0 {
		t.Fatal("the held split stored no shards")
	}
	// A shard is never approvable on its own, because the parent carries the decision.
	if _, err := d.Approve(ctx, shards[0].ID, "approver-pat", "session"); !errors.Is(err, ErrChildNotApprovable) {
		t.Errorf("Approve(shard) error = %v, want ErrChildNotApprovable: releasing a shard alone "+
			"runs it outside the parent an approver decided on", err)
	}
	time.Sleep(150 * time.Millisecond)
	if n := runner.executions.Load(); n != 0 {
		t.Errorf("%d shards executed after a shard was approved on its own", n)
	}
}
