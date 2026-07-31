package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// TestParentMayStartRefusesARequestedCancel checks the start fence sees a cancel that was requested
// rather than one that settled the run.
//
// Cancel is recorded two different ways depending on whether anything holds the run. An unclaimed
// run is settled outright, which a status comparison catches. A held run gets a flag for its holder
// to act on, and the status does not move. Approving a parent claims it, so from that moment every
// cancel takes the flag path: the fence compared only the status, saw the run it expected, and
// started a pipeline that a person had already canceled and that the API had already answered was
// canceling. It executed on real hosts and settled as succeeded.
//
// The check belongs in the same statement that makes the claim. Reading the flag first and swapping
// second leaves the same window one scheduling delay wide.
func TestParentMayStartRefusesARequestedCancel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name   string
		Status run.Status
		Kind   string
	}{{ // Test 0: A pipeline approved and then canceled before its coordinator ran.
		Name: "approved pipeline", Status: run.StatusRunning, Kind: run.KindPipeline,
	}, { // Test 1: A split canceled while its coordinator was still starting.
		Name: "submitted split", Status: run.StatusPending, Kind: run.KindSplit,
	}}
	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			store := run.NewMemStore()
			runner := &countingRunnerLister{hosts: []string{"web01", "web02"}}
			d := New(store, runner, nil, WithNoJanitor())
			defer d.Close()
			ctx := context.Background()

			held := time.Now()
			parent := &run.Run{
				ID: "run_fence", Playbook: "site.yml", Kind: test.Kind, Status: test.Status,
				CreatedAt: time.Now(), ClaimedBy: d.Owner(), ClaimedAt: &held,
				CancelRequested: true,
			}
			if err := store.Save(ctx, parent); err != nil {
				t.Fatalf("test %d: Save() error = %v", testNum, err)
			}
			if d.parentMayStart(parent.Clone(), nil) {
				t.Errorf("test %d: started a %s whose cancel was already requested",
					testNum, test.Kind)
			}
			if n := runner.executions.Load(); n != 0 {
				t.Errorf("test %d: %d executions on a canceled run", testNum, n)
			}
		})
	}
}

// TestParentMayStartStillStartsHealthyWork checks the cancel fence did not close the ordinary path.
// A parent with no cancel requested is claimed and started.
func TestParentMayStartStillStartsHealthyWork(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	d := New(store, &countingRunnerLister{hosts: []string{"web01"}}, nil, WithNoJanitor())
	defer d.Close()
	ctx := context.Background()

	parent := &run.Run{
		ID: "run_ok", Playbook: "site.yml", Kind: run.KindSplit, Status: run.StatusPending,
		CreatedAt: time.Now(),
	}
	if err := store.Save(ctx, parent); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if !d.parentMayStart(parent.Clone(), nil) {
		t.Fatal("a healthy parent was refused, so no split can ever start")
	}
	got, err := store.Get(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusRunning || got.ClaimedBy != d.Owner() {
		t.Errorf("parent is %q held by %q, want running and claimed", got.Status, got.ClaimedBy)
	}
}

// TestCancelChildrenSettlesHeldShards checks a shard waiting on an approval is settled when its
// parent goes away, rather than left in the queue carrying a flag nothing will act on.
//
// Finalizing only an unclaimed pending child left a pending_approval shard stranded permanently: no
// executor holds it so nothing reads its cancel flag, and orphan resolution fires only for an
// interrupted parent. It sat in the approval queue with its parent gone, and approving it ran it.
func TestCancelChildrenSettlesHeldShards(t *testing.T) {
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
	d.cancelChildren([]string{shard.ID})

	got, err := store.Get(ctx, shard.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !got.Status.Terminal() {
		t.Errorf("held shard is %q, so it waits in the approval queue under a parent that is gone",
			got.Status)
	}
}
