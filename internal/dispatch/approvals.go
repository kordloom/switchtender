package dispatch

import (
	"context"
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
	ok, err := d.store.TransitionStatus(ctx, id, run.StatusPendingApproval, run.StatusPending)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotPendingApproval
	}
	r.Status = run.StatusPending
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
	for _, s := range shards {
		// The swap reporting no change means the shard already moved on, which the parent's own
		// compare-and-swap has already made impossible for a second approval. Only a real error
		// stops the start.
		if _, err := d.store.TransitionStatus(ctx, s.ID,
			run.StatusPendingApproval, run.StatusPending); err != nil {
			d.log.Error("dispatch: release shard " + s.ID + ": " + err.Error())
			d.finalize(parent, run.StatusFailed, nil, "could not release the shards to run")
			return
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
