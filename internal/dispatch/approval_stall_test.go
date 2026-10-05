package dispatch

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// stallHold is a park hook that holds a workflow's coordinator in the moment between its approval
// step being listed and its park, the moment a process that dies leaves the workflow running under
// a lease nobody renews.
type stallHold struct {
	// reached receives the workflow's id once its coordinator is held.
	reached chan string
	// release lets the held coordinator go on to its park.
	release chan struct{}
	// once closes release once.
	once sync.Once
}

// newStallHold returns a hold that has not been reached.
func newStallHold() *stallHold {
	return &stallHold{reached: make(chan string, 1), release: make(chan struct{})}
}

// free lets the held coordinator go on. Calling it again does nothing.
func (h *stallHold) free() {
	h.once.Do(func() { close(h.release) })
}

// hook is the park hook: it reports the workflow and waits to be released.
func (h *stallHold) hook(id string) {
	h.reached <- id
	<-h.release
}

// wait waits for the coordinator to be held.
func (h *stallHold) wait(t *testing.T) {
	t.Helper()
	select {
	case <-h.reached:
	case <-time.After(waitBudget):
		t.Fatal("the workflow never reached its park")
	}
}

// stallApproval is the decision a person makes approving a step in these tests.
func stallApproval() StepDecision {
	return StepDecision{Approve: true, By: outcome.Decider{Name: "approver", Type: "session"}}
}

// stallAgeLease makes the workflow's lease look as old as a dead process's, the way an hour of
// silence leaves it, so the next lease sweep treats it as expired.
func stallAgeLease(t *testing.T, store run.Store, id string) {
	t.Helper()
	ctx := context.Background()
	parent, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if parent.Status != run.StatusRunning || parent.ClaimedBy == "" {
		t.Fatalf("the workflow is %s under %q, want running under its coordinator", parent.Status,
			parent.ClaimedBy)
	}
	old := time.Now().Add(-time.Hour)
	parent.ClaimedAt = &old
	if err := store.Save(ctx, parent); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
}

// listedStep waits until the workflow lists its approval step named name for a decision, alone,
// and returns it.
func listedStep(t *testing.T, store run.Store, id, name string) *run.Run {
	t.Helper()
	deadline := time.Now().Add(waitBudget)
	for time.Now().Before(deadline) {
		pending, err := run.PendingApprovalSteps(context.Background(), store, id)
		if err != nil {
			t.Fatalf("PendingApprovalSteps() error = %v", err)
		}
		if len(pending) == 1 && pending[0].StepName == name {
			return pending[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("workflow %s never listed step %q alone", id, name)
	return nil
}

// stallRunner records every command and holds the one named hold until release is closed.
type stallRunner struct {
	// commandRecorder counts each command.
	commandRecorder
	// hold is the command held.
	hold string
	// started receives once the held command starts.
	started chan struct{}
	// release lets the held command finish.
	release chan struct{}
	// once closes release once.
	once sync.Once
}

// free lets the held command finish. Calling it again does nothing.
func (r *stallRunner) free() {
	r.once.Do(func() { close(r.release) })
}

// Run holds the held command until released or stopped, then records it and succeeds.
func (r *stallRunner) Run(ctx context.Context, spec roundhouse.Spec, w io.Writer) (roundhouse.Result, error) {
	if spec.Command == r.hold {
		select {
		case r.started <- struct{}{}:
		default:
		}
		select {
		case <-r.release:
		case <-ctx.Done():
		}
	}
	return r.commandRecorder.Run(ctx, spec, w)
}

// TestStalledCoordinatorLeavesAWorkflowAnotherReplicaResumed holds a coordinator in the moment
// before its park for longer than its lease, so a second replica's sweep parks the workflow and its
// approval resumes it there. The held coordinator then goes on to its park while the second replica
// is executing one branch and waiting at a second approval step on another. Its park finds the
// workflow under another lease, and it must hand the workflow over rather than withdraw the step the
// other replica is waiting at, which ended a workflow somebody else was walking.
func TestStalledCoordinatorLeavesAWorkflowAnotherReplicaResumed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	runner := &stallRunner{hold: "deploy", started: make(chan struct{}, 1),
		release: make(chan struct{})}
	hold := newStallHold()
	first := New(store, runner, nil, WithNoJanitor(), WithOwner("first"), WithParkHook(hold.hook))
	t.Cleanup(first.Close)
	t.Cleanup(hold.free)
	parent, err := first.SubmitPipeline(ctx, "release", "", []run.PipelineStep{
		{Name: "build", Tool: run.ToolBash, Command: "build"},
		{Name: "gate", Type: run.StepApproval, Description: "Ship it?", DependsOn: []string{"build"}},
		{Name: "deploy", Tool: run.ToolBash, Command: "deploy", DependsOn: []string{"gate"}},
		{Name: "signoff", Type: run.StepApproval, Description: "Sign off?",
			DependsOn: []string{"gate"}},
		{Name: "finish", Tool: run.ToolBash, Command: "finish",
			DependsOn: []string{"deploy", "signoff"}},
	})
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	hold.wait(t)
	gate := listedStep(t, store, parent.ID, "gate")
	if _, err := first.DecideStep(ctx, gate.ID, stallApproval()); err != nil {
		t.Fatalf("DecideStep(gate) error = %v", err)
	}
	stallAgeLease(t, store, parent.ID)

	second := New(store, runner, nil, WithOwner("second"), WithClaimInterval(20*time.Millisecond))
	t.Cleanup(second.Close)
	t.Cleanup(runner.free)
	select {
	case <-runner.started:
	case <-time.After(waitBudget):
		t.Fatal("the second replica never resumed the workflow")
	}
	signoff := listedStep(t, store, parent.ID, "signoff")

	hold.free()
	apprWaitNoCoordinator(t, first, parent.ID)
	if got, err := store.Get(ctx, signoff.ID); err != nil || got.Status != run.StatusPendingApproval {
		t.Fatalf("after the held coordinator went on, the second step = %v, %v, want it still "+
			"waiting for the replica that resumed the workflow", got.Status, err)
	}

	runner.free()
	waitParked(t, store, parent.ID)
	if _, err := second.DecideStep(ctx, signoff.ID, stallApproval()); err != nil {
		t.Fatalf("DecideStep(signoff) error = %v", err)
	}
	if got := waitTerminal(t, store, parent.ID); got.Status != run.StatusSucceeded {
		t.Errorf("workflow = %q (%s), want succeeded", got.Status, got.Error)
	}
	for _, command := range []string{"build", "deploy", "finish"} {
		if n := runner.count(command); n != 1 {
			t.Errorf("%s ran %d times, want once", command, n)
		}
	}
}

// TestSweepDoesNotResumeUnderAHeldCoordinator holds a coordinator in the moment before its park
// for longer than its lease, approves its step, and runs its own process's sweep. The sweep parks
// the workflow, and the resume it then attempts must leave the workflow to the coordinator that
// still holds it in memory, which looks for decided work once it hands the workflow over. Resuming
// under it started a second walk that the held coordinator's park then took the workflow back from.
func TestSweepDoesNotResumeUnderAHeldCoordinator(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	rec := &commandRecorder{}
	hold := newStallHold()
	d := New(store, rec, nil, WithNoJanitor(), WithOwner("only"), WithParkHook(hold.hook))
	t.Cleanup(d.Close)
	t.Cleanup(hold.free)
	parent, err := d.SubmitPipeline(ctx, "release", "", gatedWorkflow(0, true))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	hold.wait(t)
	node := listedStep(t, store, parent.ID, "gate")
	if _, err := d.DecideStep(ctx, node.ID, stallApproval()); err != nil {
		t.Fatalf("DecideStep() error = %v", err)
	}
	stallAgeLease(t, store, parent.ID)
	reporter, ok := store.(settledReporter)
	if !ok {
		t.Fatalf("%T does not implement ReclaimStaleSettled", store)
	}
	if _, _, err := reporter.ReclaimStaleSettled(ctx, leaseTTL); err != nil {
		t.Fatalf("ReclaimStaleSettled() error = %v", err)
	}
	d.sweepApprovalSteps()
	if got, err := store.Get(ctx, parent.ID); err != nil ||
		got.Status != run.StatusPendingApproval || got.ClaimedBy != "" {
		t.Fatalf("after the sweep the workflow = %v under %q, %v, want it parked and left to the "+
			"coordinator holding it", got.Status, got.ClaimedBy, err)
	}

	hold.free()
	if got := waitTerminal(t, store, parent.ID); got.Status != run.StatusSucceeded {
		t.Errorf("workflow = %q (%s), want succeeded", got.Status, got.Error)
	}
	if rec.count("deploy") != 1 || rec.count("notify") != 0 {
		t.Errorf("deploy and notify ran %d and %d times, want 1 and 0", rec.count("deploy"),
			rec.count("notify"))
	}
}
