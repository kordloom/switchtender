package dispatch

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// errApprLostAck is what a store call returns when the database committed the statement and the
// connection dropped before the answer arrived.
var errApprLostAck = errors.New("connection reset by peer")

// apprLostAckStore is a run store whose first ParkForApproval, or first resume of a parked workflow
// to running, commits and then reports errApprLostAck, the way a write does when the connection
// drops after the database committed it.
type apprLostAckStore struct {
	run.Store
	// park makes the first ParkForApproval lose its answer.
	park bool
	// resume makes the first pending_approval to running claim lose its answer.
	resume bool
	// parkCalls counts ParkForApproval calls.
	parkCalls atomic.Int32
	// once makes only the first matching call lose its answer.
	once sync.Once
}

// ParkForApproval parks, and loses the answer of the first park when armed.
func (s *apprLostAckStore) ParkForApproval(ctx context.Context, id, owner string) (bool, error) {
	ok, err := s.Store.ParkForApproval(ctx, id, owner)
	s.parkCalls.Add(1)
	if s.park && ok && err == nil && s.lose() {
		return false, errApprLostAck
	}
	return ok, err
}

// TransitionStatusAndClaim claims, and loses the answer of the first resume when armed.
func (s *apprLostAckStore) TransitionStatusAndClaim(ctx context.Context, id string, from,
	to run.Status, owner string, startedAt time.Time) (bool, error) {
	ok, err := s.Store.TransitionStatusAndClaim(ctx, id, from, to, owner, startedAt)
	if s.resume && from == run.StatusPendingApproval && ok && err == nil && s.lose() {
		return false, errApprLostAck
	}
	return ok, err
}

// lose reports true exactly once.
func (s *apprLostAckStore) lose() bool {
	lost := false
	s.once.Do(func() { lost = true })
	return lost
}

// apprWaitNoCoordinator waits until no coordinator for the workflow is registered on d, which is
// when the coordinator that walked it has returned.
func apprWaitNoCoordinator(t *testing.T, d *Dispatcher, id string) {
	t.Helper()
	deadline := time.Now().Add(waitBudget)
	for time.Now().Before(deadline) {
		d.cmu.Lock()
		_, busy := d.coordinators[id]
		d.cmu.Unlock()
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the coordinator of %s never returned", id)
}

// TestApprovalStepParkWhoseAnswerIsLostStillWaits parks a workflow at its approval step when the
// park's answer is lost after the database committed it. The park is retried; the retry finds the
// workflow already parked, matches nothing, and is read as "could not park because somebody
// canceled it", so the coordinator withdraws the waiting approval step. Nobody canceled anything,
// yet the approver can no longer decide, and the next janitor sweep finds the withdrawn step,
// resumes the workflow, and ends it canceled. The concepts page says a paused workflow is held by
// nothing in memory and that canceling it is what withdraws its step.
func TestApprovalStepParkWhoseAnswerIsLostStillWaits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := &apprLostAckStore{Store: run.NewMemStore(), park: true}
	runner := &commandRecorder{}
	d := New(store, runner, nil, WithNoJanitor(), WithAudits(audit.NewMemStore()))
	defer d.Close()
	parent, err := d.SubmitPipeline(ctx, "release", "", gatedWorkflow(0, true),
		run.WithActor("requester"))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	deadline := time.Now().Add(waitBudget)
	for store.parkCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	apprWaitNoCoordinator(t, d, parent.ID)
	d.sweepApprovalSteps()

	steps, err := run.ApprovalSteps(ctx, store, parent.ID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("ApprovalSteps() = (%d steps, %v), want the one gate", len(steps), err)
	}
	if steps[0].Status != run.StatusPendingApproval {
		got := waitTerminal(t, store, parent.ID)
		t.Fatalf("after a park whose answer was lost the approval step is %s and the workflow "+
			"ended %s, want the step still waiting for a person: nobody canceled it",
			steps[0].Status, got.Status)
	}
	if _, err := d.DecideStep(ctx, steps[0].ID, StepDecision{Approve: true,
		By: outcome.Decider{Name: "approver", Type: "session"}}); err != nil {
		t.Fatalf("DecideStep() error = %v", err)
	}
	if got := waitTerminal(t, store, parent.ID); got.Status != run.StatusSucceeded {
		t.Errorf("workflow = %s (%s), want succeeded", got.Status, got.Error)
	}
}

// TestApprovalStepResumeWhoseAnswerIsLostStillContinues approves a parked workflow's step when the
// resume's compare-and-set commits and its answer is lost. resumeParked reads the error as a resume
// that did not happen and starts no coordinator, while the workflow now reads as running under
// this replica's lease. No janitor resumes a running workflow, so it is held by nothing until the
// lease sweep interrupts it: the approval was accepted, the approver was answered with success, and
// the approve path never runs. The reliability page promises that the decision is acted on exactly
// once.
func TestApprovalStepResumeWhoseAnswerIsLostStillContinues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := &apprLostAckStore{Store: run.NewMemStore(), resume: true}
	runner := &commandRecorder{}
	d := New(store, runner, nil, WithNoJanitor(), WithAudits(audit.NewMemStore()))
	defer d.Close()
	parent, err := d.SubmitPipeline(ctx, "release", "", gatedWorkflow(0, true),
		run.WithActor("requester"))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	node := waitParked(t, store, parent.ID)
	apprWaitNoCoordinator(t, d, parent.ID)
	if _, err := d.DecideStep(ctx, node.ID, StepDecision{Approve: true,
		By: outcome.Decider{Name: "approver", Type: "session"}}); err != nil {
		t.Fatalf("DecideStep() error = %v", err)
	}
	// Every janitor sweeps for decided workflows a crash left behind, and then, a lease period
	// later, for leases nobody renews. Both run here, in that order. The lease sweep runs once a
	// walked workflow has had time to finish, its coordinator polling its steps twice a second, so
	// it ends only a workflow nothing is walking.
	d.sweepApprovalSteps()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cur, gerr := store.Get(ctx, parent.ID); gerr == nil && cur.Status.Terminal() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := store.ReclaimStale(ctx, 0); err != nil {
		t.Fatalf("ReclaimStale() error = %v", err)
	}
	got := waitTerminal(t, store, parent.ID)
	if got.Status != run.StatusSucceeded || runner.count("deploy") != 1 {
		t.Errorf("an approved workflow ended %s (%s) with deploy run %d times, want it to "+
			"continue down its approve path once", got.Status, got.Error, runner.count("deploy"))
	}
}
