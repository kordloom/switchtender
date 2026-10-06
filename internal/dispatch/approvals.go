package dispatch

import (
	"context"
	"fmt"
	"time"

	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// Approve releases a run held for approval so the claim loop can pick it up. It fails when the run is
// not awaiting approval, so a decision cannot be applied twice or to a run that already moved on.
// by names the approver in the audit chain's vocabulary, with the account they acted for. The
// decision is committed to the chain before the run is released, binding the approver to a digest
// of the exact spec released.
func (d *Dispatcher) Approve(ctx context.Context, id string, by outcome.Decider) (*run.Run, error) {
	return d.approveRun(ctx, id, RunDecision{Approve: true, By: by})
}

// approveRun is Approve with the approver's optional reason. The reason is masked, recorded with a
// hiding commitment in the decision entry, and refused for confirmation when the masker changed it.
func (d *Dispatcher) approveRun(ctx context.Context, id string, dec RunDecision) (*run.Run, error) {
	by := dec.By
	// An agent's token is capped below the role the approve route needs, so it never reaches this
	// call through the API. This is the second, independent lock, the one a workflow approval step
	// already had: whatever path reaches the dispatcher with an agent decider, a token that slipped
	// past the door or a caller that never went through it, nothing is recorded and nothing is
	// released. It is checked before the run is read, because the decider alone decides it.
	if by.Type == agentActorType {
		return nil, fmt.Errorf("%w a held run: an agent may propose work, and only an authorized "+
			"person can release it", ErrAgentApproval)
	}
	r, err := d.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	// A child is not approvable on its own. Its parent is what an approver decided on, and a child
	// released by itself runs under a parent that may be canceled, with no coordinator and nothing
	// to roll it up.
	if r.ParentID != nil {
		return nil, ErrChildNotApprovable
	}
	if err := d.refuseStartedWorkflow(ctx, r); err != nil {
		return nil, err
	}
	// A cancel already requested outranks the approval. Releasing the run anyway moved it to
	// pending, where the claim predicate then skipped it for carrying the flag: it never executed
	// and never reached a terminal state, so it sat in the queue as a run nobody could finish.
	if r.CancelRequested {
		return nil, fmt.Errorf("%w: this run was canceled while it waited for a decision",
			ErrNotPendingApproval)
	}
	// Separation of duties, checked before anything is recorded: a refused decision must leave no
	// decision entry behind, or the chain would show an approval for a run that stayed held.
	//
	// Only an approval is checked. Rejecting a change you asked for needs no second person, and
	// refusing it would leave a requester unable to withdraw their own request.
	if r.RequireDistinctApprover && sameRequester(by, r) {
		return nil, fmt.Errorf("%w: %q asked for this run, and the rule that held it requires a "+
			"different person to approve it", ErrSelfApproval, by.Name)
	}
	// A decision is only recordable while the run is actually awaiting one. This is the early answer
	// for the sequential case; the claim below is what makes it hold across replicas.
	if r.Status != run.StatusPendingApproval {
		return nil, fmt.Errorf("%w: this run is %s", ErrNotPendingApproval, r.Status)
	}
	// The reason is settled before anything is recorded: a reason the rule requires and was not
	// given, one over the cap, or one the masker changed that the approver has not confirmed leaves
	// no decision behind.
	reason, masked, err := d.prepareReason(ctx, r, dec.Reason, dec.ConfirmedMask)
	if err != nil {
		return nil, err
	}
	if err := requireReason(r, reason, true); err != nil {
		return nil, err
	}
	rec, err := d.newDecisionRecord(r, nil, "approved", reason, masked, by)
	if err != nil {
		return nil, fmt.Errorf("record the approval decision: %w", err)
	}
	if dec.Comment != nil {
		c := *dec.Comment
		rec.Comment = &c
	}
	// A parent goes straight to running, never through pending.
	//
	// The abandoned-parent sweep interrupts a split or pipeline parent that is pending, unclaimed,
	// and older than the cutoff, and it measures age from CreatedAt. For a run held for a person,
	// CreatedAt is the submit time, so the moment an approval flipped it to pending it was already
	// hours past the cutoff and the very next janitor tick interrupted it and canceled every shard.
	// The approved run then executed nothing. Skipping the pending state removes the window rather
	// than narrowing it: the sweep never sees a state it can act on, and a coordinator that dies
	// later is still caught, by the lease sweep that already handles exactly that. The status change
	// and the lease are one write for the same reason, and a plain run is released to pending
	// unleased, so the claim loop can take it.
	target := run.StatusPending
	if r.Kind == run.KindSplit || r.Kind == run.KindPipeline {
		target = run.StatusRunning
	}
	c := &decisionClaim{ID: rec.ID, Verdict: "approved", Status: target}
	if d.audits != nil {
		// The decision entry binds the approver to a digest of the exact spec released, and the
		// binding over the spec as written is what execution is held to. Both are fixed now and
		// stamped before the run is released, so the executor can refuse a spec that changed
		// underneath the decision.
		entry, specDigest, err := outcome.DecisionEntry(r, "approved", by, d.now,
			outcome.ExtrasOf(rec))
		if err != nil {
			return nil, fmt.Errorf("record the approval decision: %w", err)
		}
		binding, err := outcome.SpecBinding(r)
		if err != nil {
			return nil, fmt.Errorf("bind the approved spec: %w", err)
		}
		c.Entry, c.Digest, c.Binding = claimEntryOf(entry), specDigest, binding
	}
	// The decision claims the run first and is recorded and applied only once it has won, so a
	// decision that loses, to a second approver, a rejection, a cancel, or anything on another
	// replica, is answered 409 having written nothing. A decision appended before the
	// compare-and-set that decided it stayed on the chain after it lost, stamped after the outcome
	// of a run it never released, and the bundle verifier read that as a gate bypass: an honest
	// install reported NOT VERIFIED because an approver clicked a moment too late.
	mine, err := d.decide(ctx, id, rec, c)
	if err != nil {
		return nil, decisionError("approval decision", err)
	}
	released, err := d.storeGetWithRetries(ctx, id)
	if err != nil {
		return nil, err
	}
	// No claim loop picks up a parent run of either kind, so an approved one starts on the process
	// that settled the decision or never runs at all. That is this one unless a janitor finished
	// the decision first, in which case the janitor's process started it.
	if mine {
		d.afterRunDecision(ctx, released, c)
	}
	return released, nil
}

// startSplit begins coordinating an approved split. Its shards were stored held alongside it, so
// they are released to pending here and the coordinator rolls them up. A parent whose shards are
// gone cannot run, so it fails with a stated reason rather than sitting pending forever with no
// explanation.
func (d *Dispatcher) startSplit(ctx context.Context, parent *run.Run) {
	shards, err := d.store.Shards(ctx, parent.ID)
	if err != nil {
		d.log.Error("dispatch: list shards of an approved split: " + err.Error())
		d.finalize(parent, run.StatusFailed, nil, "could not read the shards to start")
		return
	}
	if len(shards) == 0 {
		d.finalize(parent, run.StatusFailed, nil, "split has no stored shards to run")
		return
	}
	// A split whose fan-out could not be settled when it was stored holds fewer shards than it
	// counts. Starting it would run part of what the approver released and roll that up as the
	// whole, so its shards are canceled and it fails with the count instead.
	if parent.ShardCount != nil && len(shards) < *parent.ShardCount {
		for _, s := range shards {
			if _, err := d.store.CancelPending(ctx, s.ID); err != nil {
				d.log.Error("dispatch: cancel shard " + s.ID + " of an incomplete split: " + err.Error())
			}
		}
		d.finalize(parent, run.StatusFailed, nil, fmt.Sprintf("only %d of %d shards were stored, so "+
			"running it would change part of what was approved: submit it again",
			len(shards), *parent.ShardCount))
		return
	}
	// A shard that fails to release is logged and the rest proceed.
	//
	// Returning here instead left the shards already released claimable and running, under a parent
	// marked failed that no coordinator was watching and that the orphan sweep does not cover, since
	// that only fires for an interrupted parent. Part of the fan-out executed on real hosts while
	// the API reported a failure. Starting the coordinator over whatever released is the outcome
	// that keeps the rollup honest, and a shard left held is settled by the run's own cancel path.
	// The release retries, because on SQLite contention here is expected rather than exceptional and
	// every other write on this path already retries. A shard that still cannot be released is settled
	// with a stated reason rather than left held: a shard in pending_approval is unclaimable, the
	// coordinator waits on its children with no timeout, and the parent is leased and heartbeating so no
	// sweep reaches it, so one lost statement left the whole split running forever with a log line as
	// the only explanation, until somebody canceled the parent by hand.
	for _, s := range shards {
		err := withRetries(func() error {
			_, terr := d.store.TransitionStatus(ctx, s.ID,
				run.StatusPendingApproval, run.StatusPending)
			return terr
		})
		if err == nil {
			continue
		}
		d.log.Error("dispatch: release shard " + s.ID + ": " + err.Error())
		d.finalize(s, run.StatusFailed, nil,
			"could not be released for execution when the split was approved: "+err.Error())
	}
	d.wake()
	d.wg.Add(1)
	go d.coordinate(parent.Clone(), shards)
}

// startPipeline begins executing an approved pipeline. A parent whose steps were never stored cannot
// be run, so it fails with a stated reason rather than sitting pending forever with no explanation.
func (d *Dispatcher) startPipeline(parent *run.Run) {
	if len(parent.Steps) == 0 {
		d.finalize(parent, run.StatusFailed, nil, "pipeline has no stored steps to run")
		return
	}
	d.wg.Add(1)
	go d.runPipeline(parent.Clone())
}

// Reject terminally denies a run held for approval so it never executes. reason is recorded as the
// run's error; a blank reason becomes a default. by names the decider, with the account they acted
// for, and the rejection is committed to the chain before the run is settled, binding the decider
// to the spec refused.
func (d *Dispatcher) Reject(ctx context.Context, id, reason string, by outcome.Decider) (*run.Run, error) {
	return d.rejectRun(ctx, id, RunDecision{Reason: reason, By: by})
}

// rejectRun is Reject with the decision's reason handled the way an approval's is: masked, recorded
// with a hiding commitment, and refused for confirmation when the masker changed it. The reason is
// kept in the decision record, never in the run's error, so a redaction can remove it.
func (d *Dispatcher) rejectRun(ctx context.Context, id string, dec RunDecision) (*run.Run, error) {
	by := dec.By
	r, err := d.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	// A shard or step is decided through its parent, the same way it is approved. Rejecting one
	// alone leaves the rest of the fan-out to run without it, which is not a decision anyone made.
	if r.ParentID != nil {
		return nil, ErrChildNotApprovable
	}
	if err := d.refuseStartedWorkflow(ctx, r); err != nil {
		return nil, err
	}
	// Same precondition as Approve, and the same reason. A reject arriving after the run had already
	// been approved and succeeded was refused with 409 and still wrote DECISION .../rejected into the
	// chain, so the permanent record said an approver rejected a run that ran and succeeded.
	if r.Status != run.StatusPendingApproval {
		return nil, fmt.Errorf("%w: this run is %s", ErrNotPendingApproval, r.Status)
	}
	text, masked, err := d.prepareReason(ctx, r, dec.Reason, dec.ConfirmedMask)
	if err != nil {
		return nil, err
	}
	if err := requireReason(r, text, false); err != nil {
		return nil, err
	}
	rec, err := d.newDecisionRecord(r, nil, "rejected", text, masked, by)
	if err != nil {
		return nil, fmt.Errorf("record the rejection decision: %w", err)
	}
	// The run's error says it was rejected and nothing more. The reason lives in the decision record
	// beside the commitment the chain holds, where a redaction can remove it: written here, it would
	// be committed with the run's outcome and disclosed by every receipt drawn from it afterward.
	c := &decisionClaim{ID: rec.ID, Verdict: "rejected", Status: run.StatusRejected,
		Error: "rejected by an approver", Finalize: true}
	if d.audits != nil {
		entry, _, err := outcome.DecisionEntry(r, "rejected", by, d.now, outcome.ExtrasOf(rec))
		if err != nil {
			return nil, fmt.Errorf("record the rejection decision: %w", err)
		}
		c.Entry = claimEntryOf(entry)
	}
	// Claimed before it is recorded, as an approval is, so a rejection that loses to an approval
	// that already ran never writes a rejection of a run that succeeded into the permanent record.
	mine, err := d.decide(ctx, id, rec, c)
	if err != nil {
		return nil, decisionError("rejection decision", err)
	}
	rejected, err := d.storeGetWithRetries(ctx, id)
	if err != nil {
		return nil, err
	}
	// A held split stores its shards held alongside it, so the process that settled the rejection
	// settles them too, and commits the run's outcome. A pipeline creates no step runs until it
	// starts, so it has nothing to settle.
	if mine {
		d.afterRunDecision(ctx, rejected, c)
	}
	return rejected, nil
}

// rejectShards cancels the held shards of a rejected split so none is left awaiting a decision that
// has already been made. A shard that cannot be settled is logged rather than failing the rejection:
// the parent is what an approver denied, and it must end rejected either way.
func (d *Dispatcher) rejectShards(ctx context.Context, parent *run.Run, reason string) {
	shards, err := d.store.Shards(ctx, parent.ID)
	if err != nil {
		d.log.Error("dispatch: list shards of a rejected split: " + err.Error())
		return
	}
	for _, s := range shards {
		if s.Status != run.StatusPendingApproval && s.Status != run.StatusPending {
			continue
		}
		ended := time.Now()
		s.Status = run.StatusCanceled
		s.EndedAt = &ended
		s.Error = "canceled: " + reason
		if err := d.store.Save(ctx, s); err != nil {
			d.log.Error("dispatch: cancel shard " + s.ID + " of a rejected split: " + err.Error())
		}
	}
}

// refuseStartedWorkflow refuses a whole-run decision on a workflow that already started. Such a
// workflow is held only because it parked at an approval step, and approving it as a run would
// start its coordinator from the top: every step that already ran would run again, under a decision
// about a different question than the one the step is asking. Rejecting it as a run would end it
// without answering the step either. The step is decided instead.
func (d *Dispatcher) refuseStartedWorkflow(ctx context.Context, r *run.Run) error {
	if r.Kind != run.KindPipeline {
		return nil
	}
	started, err := run.Started(ctx, d.store, r.ID)
	if err != nil {
		return err
	}
	if started {
		return ErrStepPending
	}
	return nil
}
