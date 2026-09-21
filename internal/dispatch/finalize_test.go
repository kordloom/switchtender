package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// TestFinalizeDoesNotResurrectInterrupted verifies that a worker finalizing a run the janitor already
// interrupted cannot overwrite the interrupt with a terminal success. This is the fencing guarantee: a
// slow but still alive worker that loses its lease must not resurrect a reclaimed run.
func TestFinalizeDoesNotResurrectInterrupted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, okRunner(), nil)

	ended := time.Now()
	interrupted := &run.Run{
		ID: "run_x", Playbook: "p", Status: run.StatusInterrupted,
		CreatedAt: time.Now(), EndedAt: &ended, Error: "interrupted: executor lease expired",
	}
	if err := store.Save(ctx, interrupted); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// The worker's own view still thinks the run is running when it tries to finalize it as succeeded.
	worker := interrupted.Clone()
	worker.Status = run.StatusRunning
	code := 0
	d.finalize(worker, run.StatusSucceeded, &code, "")

	got, err := store.Get(ctx, "run_x")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusInterrupted {
		t.Errorf("status = %q, want interrupted (finalize must not resurrect)", got.Status)
	}
	if got.ExitCode != nil {
		t.Errorf("exit code = %v, want nil (no success recorded)", *got.ExitCode)
	}
	// finalize reflects the stored reality back to the caller's run value.
	if worker.Status != run.StatusInterrupted {
		t.Errorf("caller status = %q, want interrupted", worker.Status)
	}
}

// TestFinalizeFromRunningSucceeds verifies the normal path: a run in running finalizes to its terminal
// status with exit code and end time recorded.
func TestFinalizeFromRunningSucceeds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, okRunner(), nil)

	started := time.Now()
	r := &run.Run{
		ID: "run_y", Playbook: "p", Status: run.StatusRunning,
		CreatedAt: time.Now(), StartedAt: &started,
	}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	code := 0
	d.finalize(r, run.StatusSucceeded, &code, "")

	got, err := store.Get(ctx, "run_y")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", got.Status)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", got.ExitCode)
	}
	if got.EndedAt == nil {
		t.Error("ended_at = nil, want set")
	}
}

// TestFinalizeRejectFromPendingApproval verifies a legitimate finalize from a non running state is not
// fenced: rejecting a held run records the rejection even though it never ran.
func TestFinalizeRejectFromPendingApproval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, okRunner(), nil)

	// Reject transitions pending_approval to rejected in the store, then finalizes to record the
	// reason. finalize sees the stored status already equal to the target, so it is not fenced.
	r := &run.Run{
		ID: "run_z", Playbook: "p", Status: run.StatusRejected, CreatedAt: time.Now(),
	}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	d.finalize(r, run.StatusRejected, nil, "rejected by an approver")

	got, err := store.Get(ctx, "run_z")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusRejected {
		t.Errorf("status = %q, want rejected", got.Status)
	}
	if got.Error != "rejected by an approver" {
		t.Errorf("error = %q, want the rejection reason recorded", got.Error)
	}
}

// TestALostWorkerCannotTerminalizeARunAnotherWorkerReclaimed is the live-run half of the fence,
// which the terminal-state guard above never covered.
//
// Worker A's lease lapses mid-execution, the janitor requeues the run, and worker B claims it and
// is running it. A's late finalize is refused by the owner-fenced FinalizeRunning and falls into
// the fallback, which used to refuse only stored TERMINAL states: B's run is running, so A's
// whole-row save went through, recording canceled with A as the holder. B's next heartbeat found
// itself disowned and killed its own tool, so both executions died partway on real hosts and the
// record read as a person canceling. A run held by a different owner is not this worker's to
// finalize, live or otherwise.
func TestALostWorkerCannotTerminalizeARunAnotherWorkerReclaimed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, okRunner(), nil)

	started := time.Now()
	reclaimed := &run.Run{
		ID: "run_x", Playbook: "p", Status: run.StatusRunning,
		CreatedAt: time.Now(), StartedAt: &started, ClaimedBy: "worker-b",
	}
	if err := store.Save(ctx, reclaimed); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// Worker A's stale view: it believes it still owns the run it started.
	stale := reclaimed.Clone()
	stale.ClaimedBy = "worker-a"
	d.finalize(stale, run.StatusCanceled, nil, "")

	got, err := store.Get(ctx, "run_x")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusRunning {
		t.Errorf("status = %q, want running: a disowned worker terminalized the run its "+
			"replacement is live inside", got.Status)
	}
	if got.ClaimedBy != "worker-b" {
		t.Errorf("claimed_by = %q, want worker-b: the stale worker took the lease back", got.ClaimedBy)
	}
}
