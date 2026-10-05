package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// TestACancelDuringAClaimedApprovalEndsTheRun cancels a held run while an approval that already
// claimed it is still being recorded. The cancel cannot settle a claimed run, so it falls through
// to the cooperative flag, the way the cancel route does. The approval then takes effect and
// releases the run, and the release honors the flag: the run ends canceled before it executes.
// Released without it, the run sat pending with a flag the claim loop skips, never executing and
// never ending.
func TestACancelDuringAClaimedApprovalEndsTheRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	audits := apprNewGatedAudits(audit.NewMemStore(), "/decision/approved")
	runner := &commandRecorder{}
	d := New(store, runner, nil, WithNoJanitor(), WithAudits(audits))
	defer d.Close()
	held, err := d.Submit(ctx, "play.yml", "inv", run.WithRequireApproval(true),
		run.WithActor("requester"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	approved := make(chan error, 1)
	go func() {
		_, aerr := d.DecideRun(ctx, held.ID, RunDecision{Approve: true,
			By: outcome.Decider{Name: "approver", Type: "session"}})
		approved <- aerr
	}()
	apprWait(t, audits.reached, "the approval to claim the run")
	if done, err := d.CancelWaiting(ctx, held.ID); err != nil || done {
		t.Fatalf("CancelWaiting() of a claimed run = (%v, %v), want (false, nil)", done, err)
	}
	if err := store.RequestCancel(ctx, held.ID); err != nil {
		t.Fatalf("RequestCancel() error = %v", err)
	}
	close(audits.release)
	if err := <-approved; err != nil {
		t.Fatalf("DecideRun() error = %v", err)
	}
	if got := waitTerminal(t, store, held.ID); got.Status != run.StatusCanceled {
		t.Errorf("the run ended %s, want canceled", got.Status)
	}
	if n := runner.count("play.yml"); n != 0 {
		t.Errorf("the run executed %d times, want none", n)
	}
}

// TestAWorkflowCanceledMidDecisionRecordsTheStepFirst cancels a parked workflow on one replica
// while an approval of its step, made on another, has claimed the step and not yet been recorded.
// The cancel cannot withdraw a claimed step, so it finishes that decision before it records the
// workflow's end. The chain then holds the step's approval ahead of the workflow's outcome, and the
// whole-chain bundle verifies. Recorded after the outcome, the approval read as a gate bypass on an
// install where nothing ran without one.
func TestAWorkflowCanceledMidDecisionRecordsTheStepFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	audits := audit.NewMemStore()
	gated := apprNewGatedAudits(audits, "/decision/"+outcome.StepApproved)
	runner := &commandRecorder{}
	replicaA := New(store, runner, nil, WithNoJanitor(), WithAudits(gated),
		WithOwner("replica-a"))
	defer replicaA.Close()
	replicaB := New(store, runner, nil, WithNoJanitor(), WithAudits(audits),
		WithOwner("replica-b"))
	defer replicaB.Close()
	parent, err := replicaB.SubmitPipeline(ctx, "release", "", gatedWorkflow(0, true),
		run.WithActor("requester"))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	node := waitParked(t, store, parent.ID)
	decided := make(chan error, 1)
	go func() {
		_, derr := replicaA.DecideStep(ctx, node.ID, StepDecision{Approve: true,
			By: outcome.Decider{Name: "approver", Type: "session"}})
		decided <- derr
	}()
	apprWait(t, gated.reached, "the approval to claim the step")
	if done, err := replicaB.CancelWaiting(ctx, parent.ID); err != nil || !done {
		t.Fatalf("CancelWaiting() of the workflow = (%v, %v), want (true, nil)", done, err)
	}
	close(gated.release)
	if derr := <-decided; derr != nil && !errors.Is(derr, ErrNotPendingApproval) {
		t.Fatalf("DecideStep() error = %v", derr)
	}
	apprWaitOutcome(t, audits, parent.ID)
	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	var approvals, outcomes []int64
	for _, e := range chain {
		if _, _, verdict, ok := outcome.ParseStepDecisionPath(e.Path); ok &&
			verdict == outcome.StepApproved {
			approvals = append(approvals, e.Seq)
		}
		if e.Method == audit.MethodRun && strings.HasPrefix(e.Path, "/runs/"+parent.ID+"/outcome/") {
			outcomes = append(outcomes, e.Seq)
		}
	}
	if len(approvals) != 1 || len(outcomes) != 1 || approvals[0] > outcomes[0] {
		t.Errorf("the step approval is at %v and the workflow's outcome at %v, want one of each "+
			"with the approval first", approvals, outcomes)
	}
	if rep := apprFleetBundle(t, audits); !rep.OK() {
		t.Errorf("the whole-chain bundle reports NOT VERIFIED (approval precedes run %v): %q",
			rep.ApprovalPrecedesRun, rep.TimeProblems)
	}
}
