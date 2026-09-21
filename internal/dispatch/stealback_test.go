package dispatch

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAStalledClaimantCannotStealBackARequeuedRun pins the fence at the start of execution.
//
// A worker that wins a claim and then stalls past the lease, a paused VM, a frozen cgroup, a
// long GC, wakes up holding a run the janitor has requeued and another worker may already be
// executing. The old start was a blind whole-row save: the woken worker wrote its own claim over
// the live one and started its tool too, and the same playbook ran twice against the same hosts
// concurrently, which for a control plane is the cardinal sin. The start is now the store's
// fenced transition from pending, so a run that is no longer pending, because somebody else
// legitimately owns it, is walked away from without a write, without a tool, and without a
// finalize that would stomp the owner's terminal state.
func TestAStalledClaimantCannotStealBackARequeuedRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()

	var executions atomic.Int32
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			executions.Add(1)
			return roundhouse.Result{ExitCode: 0}, nil
		})
	d := New(store, runner, nil, WithOwner("worker-A"))
	defer d.Close()

	// The run as worker-A saw it the moment it won the claim: pending, leased to A.
	now := time.Now()
	seed := &run.Run{ID: "run_steal", Playbook: "site.yml", Status: run.StatusPending,
		ClaimedBy: "worker-A", CreatedAt: now, Tool: run.ToolBash, Queue: "q-isolated", Command: "echo once"}
	if err := store.Save(ctx, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	stale := seed.Clone()

	// While A was stalled: the janitor requeued the run and worker-B took it and started.
	ok, err := store.TransitionStatusAndClaim(ctx, "run_steal", run.StatusPending,
		run.StatusRunning, "worker-B", time.Now())
	if err != nil || !ok {
		t.Fatalf("worker-B's legitimate claim: ok=%v err=%v", ok, err)
	}

	// A wakes up and does what its execution path does.
	finished := atomic.Int32{}
	d.streamSpec(ctx, stale, false, nil,
		func(roundhouse.Result, error, *masker, *run.SummaryFold) run.Status {
			finished.Add(1)
			return run.StatusSucceeded
		})

	if n := executions.Load(); n != 0 {
		t.Fatalf("the stalled claimant executed the tool %d time(s): the same change is running "+
			"twice against the same hosts", n)
	}
	if n := finished.Load(); n != 0 {
		t.Fatalf("the stalled claimant finalized %d time(s) over the live owner's run", n)
	}
	got, err := store.Get(ctx, "run_steal")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ClaimedBy != "worker-B" || got.Status != run.StatusRunning {
		t.Fatalf("the live owner's claim was stomped: status=%s claimed_by=%s, want "+
			"running/worker-B untouched", got.Status, got.ClaimedBy)
	}
}

// TestAStaleCancelCannotStompARequeuedRun pins the fallback against the requeue window. A worker
// that loses its lease mid-run kills its own tool and finalizes canceled; if the janitor had
// already requeued the run, that late cancel used to land on a pending, unclaimed row and end a
// run another worker was about to pick up, recording a cancellation nobody asked for over a
// retry the product had already promised. A requeued run belongs to nobody, and nobody's runs
// are not this worker's to finalize.
func TestAStaleCancelCannotStompARequeuedRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, okRunner(), nil, WithOwner("worker-A"))
	defer d.Close()

	// The requeued state: pending, claim cleared by the janitor.
	now := time.Now()
	seed := &run.Run{ID: "run_requeued", Playbook: "site.yml", Status: run.StatusPending,
		CreatedAt: now, Tool: run.ToolBash, Queue: "q-isolated", Command: "echo again"}
	if err := store.Save(ctx, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The worker's stale view: it still thinks it runs this, claimed and running.
	stale := seed.Clone()
	stale.Status = run.StatusRunning
	stale.ClaimedBy = "worker-A"

	d.finalize(stale, run.StatusCanceled, nil, "canceled by lease loss")

	got, err := store.Get(ctx, "run_requeued")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != run.StatusPending {
		t.Fatalf("the requeued run reads %q: a stale cancel ended a retry the janitor had "+
			"already promised", got.Status)
	}
}

// heartbeatLostStore answers every heartbeat with ErrNotFound while leaving the row itself
// untouched, so the executor's lease-loss path fires while its finalize can still land.
type heartbeatLostStore struct {
	run.Store
}

// Heartbeat always reports the lease gone.
func (heartbeatLostStore) Heartbeat(context.Context, string, string) error {
	return run.ErrNotFound
}

// TestALostLeaseRecordsAnInterruptionNotACancel pins what the record says when an executor's own
// heartbeats discover its lease is gone. It used to route through the user-cancel path and write
// canceled with an empty error: a decision nobody made, over what is an interruption, the exact
// state a rerun resumes and the same status the janitor's reclaim writes for the same event seen
// from the other side.
func TestALostLeaseRecordsAnInterruptionNotACancel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	store := heartbeatLostStore{backing}
	block := make(chan struct{})
	runner := roundhouse.RunnerFunc(
		func(rctx context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			close(block)
			<-rctx.Done()
			return roundhouse.Result{ExitCode: -1}, rctx.Err()
		})
	d := New(store, runner, nil, WithOwner("worker-A"), WithNoJanitor())
	defer d.Close()

	now := time.Now()
	seed := &run.Run{ID: "run_leaselost", Playbook: "site.yml", Status: run.StatusPending,
		ClaimedBy: "worker-A", CreatedAt: now, Tool: run.ToolBash, Queue: "q-isolated",
		Command: "sleep 60"}
	if err := backing.Save(ctx, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	done := make(chan run.Status, 1)
	go func() { done <- d.executeLeased(ctx, seed.Clone()) }()
	<-block

	select {
	case got := <-done:
		if got != run.StatusInterrupted {
			t.Fatalf("executeLeased returned %q, want interrupted", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the executor never stood down after losing its lease")
	}
	stored, err := backing.Get(ctx, "run_leaselost")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status != run.StatusInterrupted {
		t.Fatalf("stored status = %q, want interrupted", stored.Status)
	}
	if stored.Error == "" || !strings.Contains(stored.Error, "lost its lease") {
		t.Fatalf("stored error = %q, want the lease loss stated: an empty reason reads as a "+
			"cancel somebody chose", stored.Error)
	}
}
