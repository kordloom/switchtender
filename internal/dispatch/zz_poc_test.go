package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// TestPoCRetryBypassesApprovalAfterReject proves an operator can execute a split an approver
// explicitly rejected, by retrying its "failed" shards.
func TestPoCRetryBypassesApprovalAfterReject(t *testing.T) {
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
		t.Fatalf("parent status = %q, want pending_approval", parent.Status)
	}

	// An approver says no.
	if _, err := d.Reject(ctx, parent.ID, "not on prod"); err != nil {
		t.Fatalf("Reject() error = %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := runner.executions.Load(); n != 0 {
		t.Fatalf("executions after reject = %d, want 0", n)
	}

	// The operator, who only needs the operator role for POST /runs/{id}/retry, retries.
	retry, err := d.RetryFailedShards(ctx, parent.ID)
	if err != nil {
		t.Fatalf("RetryFailedShards() error = %v", err)
	}
	t.Logf("retry parent status = %q (want pending_approval if the gate held)", retry.Status)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runner.executions.Load() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := runner.executions.Load(); n > 0 {
		t.Fatalf("BYPASS: %d shard executions ran after the approver rejected the split", n)
	}
}
