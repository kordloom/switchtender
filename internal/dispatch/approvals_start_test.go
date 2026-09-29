package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// TestApprovedParentWithNothingToRunFailsWithAReason pins what happens when an approval releases a
// coordinator whose work is missing. No claim loop ever picks up a split or pipeline parent, so a
// parent released into running with no shards and no steps would sit running forever, holding a lease
// and answering nothing, until somebody canceled it by hand. Failing it with a stated reason is what
// makes the situation legible.
func TestApprovedParentWithNothingToRunFailsWithAReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says which kind of parent is empty.
		Name string
		// Kind is the parent's coordination kind.
		Kind string
		// WantReason is a fragment the failure text must carry.
		WantReason string
	}{{ // Test 0: A split whose shards are gone has no host groups to run.
		Name: "a split with no shards", Kind: run.KindSplit, WantReason: "shards",
	}, { // Test 1: A pipeline whose steps were never stored has no graph to walk.
		Name: "a pipeline with no steps", Kind: run.KindPipeline, WantReason: "steps",
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			runner := &countingRunnerLister{hosts: []string{"web01", "web02"}}
			d := New(store, runner, nil, WithNoJanitor(), WithClaimInterval(time.Millisecond))
			defer d.Close()

			parent := &run.Run{
				ID: fmt.Sprintf("run_empty_%d", testNum), Playbook: "site.yml", Inventory: "inv",
				Kind: test.Kind, Status: run.StatusPendingApproval, CreatedAt: time.Now(),
			}
			if err := store.Save(ctx, parent); err != nil {
				t.Fatalf("Save() error = %v", err)
			}

			if _, err := d.Approve(ctx, parent.ID, "approver-pat", "session"); err != nil {
				t.Fatalf("Approve() error = %v", err)
			}

			got := waitTerminal(t, store, parent.ID)
			if got.Status != run.StatusFailed {
				t.Errorf("status = %q, want failed: nothing will ever run this parent, and no claim "+
					"loop picks up a coordinator", got.Status)
			}
			if !strings.Contains(got.Error, test.WantReason) {
				t.Errorf("error = %q, want it to mention the missing %s", got.Error, test.WantReason)
			}
			if n := runner.executions.Load(); n != 0 {
				t.Errorf("%d executions from a parent with nothing to run", n)
			}
		})
	}
}

// TestRejectingASplitSettlesItsHeldShards pins the fan-out half of a rejection. A held split stores
// its shards held alongside it, and a shard in pending_approval is unclaimable: nothing executes it,
// nothing reads a cancel flag on it, and the orphan sweep covers only an interrupted parent. Left
// behind they sit in the approval queue forever, and approving one runs it under a parent that was
// already refused.
func TestRejectingASplitSettlesItsHeldShards(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, &countingRunnerLister{hosts: []string{"web01", "web02"}}, nil, WithNoJanitor())
	defer d.Close()

	parent, err := d.SubmitSplit(ctx, "site.yml", "hosts.ini", 2, run.WithRequireApproval(true))
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	if parent.Status != run.StatusPendingApproval {
		t.Fatalf("submitted split status = %q, want pending_approval", parent.Status)
	}

	rejected, err := d.Reject(ctx, parent.ID, "not on a Friday", "approver-pat", "session")
	if err != nil {
		t.Fatalf("Reject() error = %v", err)
	}
	if rejected.Status != run.StatusRejected {
		t.Errorf("parent status = %q, want rejected", rejected.Status)
	}

	shards, err := store.Shards(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	if len(shards) == 0 {
		t.Fatal("the split stored no shards, so this proves nothing")
	}
	for _, s := range shards {
		if !s.Status.Terminal() {
			t.Errorf("shard %s is %q after its parent was rejected, so it waits in the approval "+
				"queue for a decision that has already been made", s.ID, s.Status)
		}
		if !strings.Contains(s.Error, "not on a Friday") {
			t.Errorf("shard %s error = %q, want the rejection reason carried onto it", s.ID, s.Error)
		}
	}
}

// TestRejectWithoutAReasonRecordsADefault pins the rejection text. A rejected run is terminal
// evidence that somebody refused a change, and a blank error there reads as a run that ended for no
// stated cause, which is the record least useful to whoever reads it later.
func TestRejectWithoutAReasonRecordsADefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, okRunner(), nil, WithNoJanitor())
	defer d.Close()

	created, err := d.Submit(ctx, "play.yml", "inv", run.WithRequireApproval(true))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if _, err := d.Reject(ctx, created.ID, "", "approver-pat", "session"); err != nil {
		t.Fatalf("Reject() error = %v", err)
	}

	got, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusRejected {
		t.Fatalf("status = %q, want rejected", got.Status)
	}
	if got.Error == "" {
		t.Error("a rejected run carries no reason, so its record reads as a run that ended for no " +
			"stated cause")
	}
}

// TestApproveAndRejectOnAMissingRun pins the lookup at the top of both decisions. A decision applied
// to a run that does not exist must report that rather than committing a decision entry to the chain
// for nothing.
func TestApproveAndRejectOnAMissingRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := New(run.NewMemStore(), okRunner(), nil, WithNoJanitor())
	defer d.Close()

	if _, err := d.Approve(ctx, "run_absent", "pat", "session"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Approve() error = %v, want %v", err, run.ErrNotFound)
	}
	if _, err := d.Reject(ctx, "run_absent", "no", "pat", "session"); !errors.Is(err, run.ErrNotFound) {
		t.Errorf("Reject() error = %v, want %v", err, run.ErrNotFound)
	}
}

// TestApprovedParentGoesStraightToRunningAndOwned pins the state an approved coordinator lands in.
// Passing through pending would expose it to the abandoned-parent sweep, which measures age from
// creation, and a run held for a person is old by definition, so the very next tick would interrupt
// an approval that had just been granted. Landing running and leased in one step removes the window
// rather than narrowing it.
func TestApprovedParentGoesStraightToRunningAndOwned(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says which coordinator is approved.
		Name string
		// Kind is the parent's coordination kind.
		Kind string
	}{
		{Name: "a split", Kind: run.KindSplit},       // Test 0: An approved split.
		{Name: "a pipeline", Kind: run.KindPipeline}, // Test 1: An approved pipeline.
	}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			d := New(store, &countingRunnerLister{hosts: []string{"web01"}}, nil, WithNoJanitor())
			defer d.Close()

			parentID := fmt.Sprintf("run_coord_%d", testNum)
			parent := &run.Run{
				ID: parentID, Playbook: "site.yml", Inventory: "inv",
				Kind: test.Kind, Status: run.StatusPendingApproval, CreatedAt: time.Now(),
			}
			if test.Kind == run.KindPipeline {
				parent.Steps = []run.PipelineStep{
					{Name: "one", Tool: run.ToolBash, Command: "echo hi"},
				}
			}
			if err := store.Save(ctx, parent); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			if test.Kind == run.KindSplit {
				idx, count := 0, 1
				if err := store.Save(ctx, &run.Run{
					ID: parentID + "_c0", Playbook: "site.yml", Status: run.StatusPendingApproval,
					CreatedAt: time.Now(), ParentID: &parentID, ShardIndex: &idx, ShardCount: &count,
					Limit: "web01",
				}); err != nil {
					t.Fatalf("Save(shard) error = %v", err)
				}
			}

			got, err := d.Approve(ctx, parent.ID, "approver-pat", "session")
			if err != nil {
				t.Fatalf("Approve() error = %v", err)
			}
			if got.Status != run.StatusRunning {
				t.Errorf("returned status = %q, want running: a parent that lands in pending is "+
					"immediately eligible for the abandoned-parent sweep", got.Status)
			}
			if got.ClaimedBy != d.Owner() {
				t.Errorf("ClaimedBy = %q, want this process: a running parent with no lease is "+
					"exactly what that sweep settles", got.ClaimedBy)
			}
			if got.ClaimedAt == nil {
				t.Error("the approved parent carries no lease time, so no sweep clause reaches it")
			}
			if got.StartedAt == nil {
				t.Error("the approved parent records no start time")
			}
		})
	}
}

// TestApprovedPlainRunIsReleasedUnleased is the counterpart rule. A plain run is released to pending
// precisely so the claim loop can take it, and the loop skips anything already leased, so stamping an
// owner on one would leave an approved run that nothing ever executes.
func TestApprovedPlainRunIsReleasedUnleased(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, okRunner(), nil, WithNoJanitor())
	defer d.Close()

	held := &run.Run{
		ID: "run_plain_held", Playbook: "site.yml", Inventory: "inv",
		Status: run.StatusPendingApproval, CreatedAt: time.Now(),
	}
	if err := store.Save(ctx, held); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := d.Approve(ctx, held.ID, "approver-pat", "session")
	if err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	if got.Status != run.StatusPending {
		t.Errorf("status = %q, want pending so the claim loop can take it", got.Status)
	}

	stored, err := store.Get(ctx, held.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if stored.ClaimedBy != "" {
		t.Errorf("ClaimedBy = %q, want unleased: the claim loop skips a leased run, so an approved "+
			"run stamped with an owner is never executed", stored.ClaimedBy)
	}
}

// TestApproveIsNotAppliedTwice pins the atomic transition behind a decision. Two approvers clicking at
// once, or one double click, must not release the same run twice: the second attempt has to be
// refused rather than re-releasing a run that may already be executing.
func TestApproveIsNotAppliedTwice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, okRunner(), nil, WithNoJanitor())
	defer d.Close()

	held := &run.Run{
		ID: "run_twice", Playbook: "site.yml", Inventory: "inv",
		Status: run.StatusPendingApproval, CreatedAt: time.Now(),
	}
	if err := store.Save(ctx, held); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if _, err := d.Approve(ctx, held.ID, "approver-pat", "session"); err != nil {
		t.Fatalf("Approve(first) error = %v", err)
	}
	if _, err := d.Approve(ctx, held.ID, "approver-sam", "session"); !errors.Is(err,
		ErrNotPendingApproval) {
		t.Errorf("Approve(second) error = %v, want %v", err, ErrNotPendingApproval)
	}
	// And a rejection cannot overturn a decision that was already made.
	if _, err := d.Reject(ctx, held.ID, "changed my mind", "approver-sam",
		"session"); !errors.Is(err, ErrNotPendingApproval) {
		t.Errorf("Reject after approve: error = %v, want %v", err, ErrNotPendingApproval)
	}
}
