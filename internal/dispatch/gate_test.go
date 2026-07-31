package dispatch

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
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
	if _, err := d.Reject(ctx, parent.ID, "not on prod"); err != nil {
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

	if _, err := d.Approve(ctx, parent.ID); err != nil {
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
func (f *failingLister) Hosts(context.Context, string) ([]string, error) { return f.hosts, nil }
