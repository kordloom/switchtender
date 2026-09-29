package dispatch

import (
	"context"
	"io"
	"testing"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestParentStartKeepsItsLeaseStamp proves the write that moves a coordinated parent to running
// never erases the lease time its start fence stamped. The whole-row save writes every column, and a
// split or pipeline parent never passed through Claim, so its in-memory ClaimedAt was nil: the save
// left the row running and claimed with no lease time, and a coordinator that crashed before its
// first heartbeat stranded the parent forever, unreachable by the stale-lease sweep (needs a lease
// time), the abandoned-parent sweep (needs no claim), and the cancel API alike.
func TestParentStartKeepsItsLeaseStamp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		})
	d := New(store, runner, nil, WithWorkers(1), WithNoJanitor())
	defer d.Close()

	parent := &run.Run{ID: "run_parent", Status: run.StatusPending, Kind: run.KindPipeline,
		Playbook: "pipe", Inventory: "inv", CreatedAt: d.now()}
	if err := store.Save(ctx, parent); err != nil {
		t.Fatalf("save parent: %v", err)
	}

	// Exactly what coordinate and runPipeline do before handing off to the watch goroutine: fence,
	// then persist the running row. The watch heartbeat has not run yet, which is the crash window.
	if !d.parentMayStart(parent, nil) {
		t.Fatal("parentMayStart refused a pending parent")
	}
	d.startParentRow(parent)

	stored, err := store.Get(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status != run.StatusRunning || stored.ClaimedBy == "" {
		t.Fatalf("parent is %s claimed by %q, want running with an owner", stored.Status, stored.ClaimedBy)
	}
	if stored.ClaimedAt == nil {
		t.Fatal("the running, claimed parent has no lease time: a crash before the first " +
			"heartbeat leaves it unsweepable and unkillable forever")
	}
}
