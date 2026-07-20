package dispatch

import (
	"context"

	"github.com/dcadolph/switchtender/internal/run"
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
	return r, nil
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
