package dispatch

import (
	"context"
	"fmt"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// Approve releases a run held for approval so the claim loop can pick it up. It fails when the run is
// not awaiting approval, so a decision cannot be applied twice or to a run that already moved on.
func (d *Dispatcher) Approve(ctx context.Context, id string) (*run.Run, error) {
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
	// A cancel already requested outranks the approval. Releasing the run anyway moved it to
	// pending, where the claim predicate then skipped it for carrying the flag: it never executed
	// and never reached a terminal state, so it sat in the queue as a run nobody could finish.
	if r.CancelRequested {
		return nil, fmt.Errorf("%w: this run was canceled while it waited for a decision",
			ErrNotPendingApproval)
	}
	// A parent goes straight to running, never through pending.
	//
	// The abandoned-parent sweep interrupts a split or pipeline parent that is pending, unclaimed,
	// and older than the cutoff, and it measures age from CreatedAt. For a run held for a person,
	// CreatedAt is the submit time, so the moment an approval flipped it to pending it was already
	// hours past the cutoff and the very next janitor tick interrupted it and canceled every shard.
	// The approved run then executed nothing. Skipping the pending state removes the window rather
	// than narrowing it: the sweep never sees a state it can act on, and a coordinator that dies
	// later is still caught, by the lease sweep that already handles exactly that.
	target := run.StatusPending
	if r.Kind == run.KindSplit || r.Kind == run.KindPipeline {
		target = run.StatusRunning
	}
	// The status change and the lease are one operation.
	//
	// Doing them in sequence leaves a window either way. Transition first and the parent is running,
	// unleased, and instantly eligible for the sweep that settles parents nothing will finish, since
	// that measures age from CreatedAt and a run held for a person is old by definition. Lease first
	// and a process that dies before the transition leaves the run held with an owner, which
	// CancelPending refuses to touch, so it can never be canceled either. One atomic step has
	// neither state: the parent is pending_approval, then it is running and owned.
	// Only a parent is claimed here. A plain run is released to pending precisely so the claim loop
	// can take it, and the loop skips anything already leased, so stamping an owner on one would
	// leave an approved run that nothing ever executes.
	var ok bool
	if target == run.StatusRunning {
		ok, err = d.store.TransitionStatusAndClaim(ctx, id, run.StatusPendingApproval, target, d.owner)
	} else {
		ok, err = d.store.TransitionStatus(ctx, id, run.StatusPendingApproval, target)
	}
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotPendingApproval
	}
	r.Status = target
	if target == run.StatusRunning {
		// The store stamped the lease and the start time in the same statement, so the answer the
		// caller gets carries them too rather than describing a run that no longer exists.
		claimed := time.Now()
		r.ClaimedBy = d.owner
		r.ClaimedAt = &claimed
		if r.StartedAt == nil {
			r.StartedAt = &claimed
		}
	}
	// No claim loop picks up a parent run of either kind, so an approved one starts here or never
	// runs at all.
	switch r.Kind {
	case run.KindPipeline:
		d.startPipeline(r)
	case run.KindSplit:
		d.startSplit(ctx, r)
	default:
		// A plain run just became claimable, so the claim loop is nudged rather than left to
		// finish an idle backoff while an approved run waits.
		d.wake()
	}
	return r, nil
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
	// A shard that fails to release is logged and the rest proceed.
	//
	// Returning here instead left the shards already released claimable and running, under a parent
	// marked failed that no coordinator was watching and that the orphan sweep does not cover, since
	// that only fires for an interrupted parent. Part of the fan-out executed on real hosts while
	// the API reported a failure. Starting the coordinator over whatever released is the outcome
	// that keeps the rollup honest, and a shard left held is settled by the run's own cancel path.
	for _, s := range shards {
		if _, err := d.store.TransitionStatus(ctx, s.ID,
			run.StatusPendingApproval, run.StatusPending); err != nil {
			d.log.Error("dispatch: release shard " + s.ID + ": " + err.Error())
		}
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
	go d.runPipeline(parent.Clone(), parent.Steps)
}

// Reject terminally denies a run held for approval so it never executes. reason is recorded as the
// run's error; a blank reason becomes a default.
func (d *Dispatcher) Reject(ctx context.Context, id, reason string) (*run.Run, error) {
	r, err := d.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	// A shard or step is decided through its parent, the same way it is approved. Rejecting one
	// alone leaves the rest of the fan-out to run without it, which is not a decision anyone made.
	if r.ParentID != nil {
		return nil, ErrChildNotApprovable
	}
	ok, err := d.store.TransitionStatus(ctx, id, run.StatusPendingApproval, run.StatusRejected)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotPendingApproval
	}
	if reason == "" {
		reason = "rejected by an approver"
	}
	// A held split stores its shards held alongside it, so rejecting the parent has to settle them
	// or they sit awaiting an approval that already happened and was a no. A pipeline creates no
	// step runs until it starts, so it has nothing to settle here.
	if r.Kind == run.KindSplit {
		d.rejectShards(ctx, r, reason)
	}
	d.finalize(r, run.StatusRejected, nil, reason)
	return r, nil
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
