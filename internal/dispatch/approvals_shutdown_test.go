package dispatch

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// stopOnStepStore is a run store that stops its dispatcher the moment an approval step's record is
// written, which is the instant between the walk opening the step, where the approval queue
// already offers it, and the walk deciding to park.
type stopOnStepStore struct {
	run.Store
	// stop stops the dispatcher the way a shutdown signal does.
	stop func()
	// once makes only the first approval step's record stop it.
	once sync.Once
}

// Save saves the run and stops the dispatcher once the first approval step's record is saved.
func (s *stopOnStepStore) Save(ctx context.Context, r *run.Run) error {
	if err := s.Store.Save(ctx, r); err != nil {
		return err
	}
	if r.Kind == run.KindApproval {
		s.once.Do(s.stop)
	}
	return nil
}

// contextAudits is an audit store that refuses an append once its context has ended, the way the
// database stores do, so a write still governed by the walk's own context fails when it stops.
type contextAudits struct {
	audit.Store
}

// Append refuses the entry when its context has ended and appends it otherwise.
func (a contextAudits) Append(ctx context.Context, e *audit.Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return a.Store.Append(ctx, e)
}

// TestAShutdownAsTheStepOpensLeavesTheWorkflowParked pins that a process stopping after it saved
// an approval step, and before it parked the workflow, records the step's request and parks it. The
// step was already offered for a decision, so failing it as a request nobody could be asked, or
// withdrawing it, ended a workflow nobody decided, and the replica that came back found nothing to
// approve. A different process then approves the step and finishes the workflow from what the store
// holds, without running the finished step again.
func TestAShutdownAsTheStepOpensLeavesTheWorkflowParked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := &stopOnStepStore{Store: run.NewMemStore()}
	audits := contextAudits{audit.NewMemStore()}
	first := &commandRecorder{}
	d1 := New(store, first, nil, WithNoJanitor(), WithAudits(audits), WithOwner("replica-one"))
	stopped := make(chan struct{})
	store.stop = func() {
		d1.cancel(errShuttingDown)
		close(stopped)
	}
	parent, err := d1.SubmitPipeline(ctx, "release", "", gatedWorkflow(0, true))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(waitBudget):
		t.Fatal("the walk never saved its approval step")
	}
	// Close waits for the walk, which the stop reached between saving the step and parking.
	d1.Close()

	held, err := store.Get(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if held.Status != run.StatusPendingApproval || held.ClaimedBy != "" {
		t.Fatalf("after the shutdown the workflow is %s (%s) held by %q, want parked at its step",
			held.Status, held.Error, held.ClaimedBy)
	}
	node := waitParked(t, store.Store, parent.ID)

	second := &commandRecorder{}
	d2 := New(store.Store, second, nil, WithNoJanitor(), WithAudits(audits),
		WithOwner("replica-two"))
	defer d2.Close()
	if _, err := d2.DecideStep(ctx, node.ID, StepDecision{Approve: true,
		By: outcome.Decider{Name: "approver", Type: "session"}}); err != nil {
		t.Fatalf("DecideStep() error = %v", err)
	}
	if got := waitTerminal(t, store.Store, parent.ID); got.Status != run.StatusSucceeded {
		t.Errorf("the approved workflow = %s (%s), want succeeded", got.Status, got.Error)
	}
	if first.count("build") != 1 || second.count("build") != 0 || second.count("deploy") != 1 {
		t.Errorf("build ran %d then %d times and deploy %d times, want 1, 0, and 1",
			first.count("build"), second.count("build"), second.count("deploy"))
	}
}
