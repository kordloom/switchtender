package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// TestApproveRefusesARunAlreadyCanceled checks that a cancel taken while a run waited for a decision
// outranks the approval.
//
// Releasing it anyway moved it to pending, where the claim predicate then skipped it for carrying
// the cancel flag. It never executed and never reached a terminal state: a run sat in the queue that
// nothing would run and nothing would finish, and the approval had answered success.
func TestApproveRefusesARunAlreadyCanceled(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	d := New(store, &countingRunnerLister{hosts: []string{"web01"}}, nil, WithNoJanitor())
	defer d.Close()
	ctx := context.Background()

	held := &run.Run{
		ID: "run_held", Playbook: "site.yml", Status: run.StatusPendingApproval,
		CreatedAt: time.Now(), CancelRequested: true,
	}
	if err := store.Save(ctx, held); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := d.Approve(ctx, held.ID); err == nil {
		t.Error("approving a run whose cancel was already requested reported success, leaving a " +
			"run nothing will claim and nothing will finish")
	}
	got, err := store.Get(ctx, held.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status == run.StatusPending {
		t.Errorf("run is %q carrying a cancel, which the claim predicate skips forever", got.Status)
	}
}

// TestRejectRefusesAShard checks that a shard is decided through its parent, the same way approving
// one is. Rejecting a single shard leaves the rest of the fan-out to run without it, which is not a
// decision anybody made.
func TestRejectRefusesAShard(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	d := New(store, &countingRunnerLister{hosts: []string{"web01"}}, nil, WithNoJanitor())
	defer d.Close()
	ctx := context.Background()

	parentID := "run_parent"
	idx, count := 0, 2
	shard := &run.Run{
		ID: "run_parent_c0", Playbook: "site.yml", Status: run.StatusPendingApproval,
		CreatedAt: time.Now(), ParentID: &parentID, ShardIndex: &idx, ShardCount: &count,
	}
	if err := store.Save(ctx, shard); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := d.Reject(ctx, shard.ID, "no"); !errors.Is(err, ErrChildNotApprovable) {
		t.Errorf("Reject(shard) error = %v, want ErrChildNotApprovable", err)
	}
	if _, err := d.Approve(ctx, shard.ID); !errors.Is(err, ErrChildNotApprovable) {
		t.Errorf("Approve(shard) error = %v, want ErrChildNotApprovable", err)
	}
}
