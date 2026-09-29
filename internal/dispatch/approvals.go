package dispatch

import (
	"context"

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
	// No claim loop picks up a pipeline parent, so an approved pipeline starts here or never runs.
	if r.Kind == run.KindPipeline {
		d.startPipeline(r)
	}
	return r, nil
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
	d.finalize(r, run.StatusRejected, nil, reason)
	return r, nil
}
