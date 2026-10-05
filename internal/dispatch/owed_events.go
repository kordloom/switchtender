package dispatch

import (
	"context"
	"errors"

	"go.uber.org/zap"

	named "github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/run"
)

// announceOwed tells a top-level run's named targets about the start or the hold it is at, event
// naming which, and then settles that event in the store's ledger, so the sweep of owed starts and
// holds leaves it alone. One the outbox could not record is left owed, and the sweep records it
// once the store answers again. Without an outbox the targets are told directly, best effort, and
// there is nothing durable to wait for.
func (d *Dispatcher) announceOwed(r *run.Run, event string) {
	if d.outbox != nil && r.ID != "" {
		if d.recordNamed(r, named.Branch{}) != nil {
			return
		}
	} else {
		d.notifyNamed(r)
	}
	d.settleOwed(r.ID, event)
}

// settleOwed marks a run's start or hold announced in the store's ledger, when the store keeps one.
// A settle that fails leaves the event owed, and the sweep records it again, which the outbox keeps
// to one start and one hold per run.
func (d *Dispatcher) settleOwed(id, event string) {
	ledger, ok := d.store.(run.EventLedger)
	if !ok || id == "" {
		return
	}
	if err := withRetries(func() error {
		return ledger.SettleEvent(context.Background(), id, event)
	}); err != nil {
		d.log.Warn("dispatch: settle a run's owed "+event+" event: "+err.Error(),
			zap.String("run_id", id))
	}
}

// sweepOwedEvents records, for its named targets, the start or hold of every top-level run the
// store still holds owed past the grace, and settles each one it handled. These are the runs whose
// process stopped between the write that moved the run and the announcement, whose announcement the
// store refused for longer than the outbox asks, or whose move no announcing path followed. A run
// still where the event left it is announced. A run that has moved past it, ended or not, is not:
// its start or hold is recorded unsent, with the reason, on the run's notifications. A run gone
// from the store has nothing left to announce. Only the named targets' durable delivery is made
// here, the same as for owed ends.
func (d *Dispatcher) sweepOwedEvents(ctx context.Context, ledger run.EventLedger) {
	owed, err := ledger.OwedEvents(ctx, endGrace, endSweepBatch)
	if err != nil {
		if ctx.Err() == nil {
			d.log.Error("dispatch: list owed run starts and holds: " + err.Error())
		}
		return
	}
	for _, o := range owed {
		r, err := d.store.Get(ctx, o.RunID)
		switch {
		case errors.Is(err, run.ErrNotFound):
			// Purged by retention, so there is nothing left to announce.
		case err != nil:
			if ctx.Err() == nil {
				d.log.Error("dispatch: read a run with an owed "+o.Event+" event: "+err.Error(),
					zap.String("run_id", o.RunID))
			}
			continue
		case r.ParentID == nil:
			if d.announceOwedEvent(ctx, r, o.Event) != nil {
				continue
			}
		}
		if err := ledger.SettleEvent(ctx, o.RunID, o.Event); err != nil && ctx.Err() == nil {
			d.log.Warn("dispatch: settle a run's owed "+o.Event+" event: "+err.Error(),
				zap.String("run_id", o.RunID))
		}
	}
}

// announceOwedEvent records one owed start or hold of a top-level run for its named targets. A run
// still running is announced as started, and a run still held as held: a plain hold as the run's
// own, and a workflow parked at its approval steps as each step it waits at, the way the workflow
// announced them, since a parked workflow's hold is the wait at its steps. Each of those is
// recorded once per run, so one the run's own process recorded before it stopped is not recorded
// again. A run that has moved past the event is recorded unsent instead.
func (d *Dispatcher) announceOwedEvent(ctx context.Context, r *run.Run, event string) error {
	switch {
	case event == run.OwedStart && r.Status == run.StatusRunning:
		return d.recordNamed(r, named.Branch{})
	case event == run.OwedHold && r.Status == run.StatusPendingApproval:
		return d.announceOwedHold(ctx, r)
	case event != run.OwedStart && event != run.OwedHold:
		// Not an event this release owes, so there is nothing it could record.
		return nil
	}
	snap := redactForExternal(r)
	err := d.outbox.RecordUnsent(d.ctx, &snap, event, unsentReason(event, r.Status))
	if err != nil {
		d.log.Error("dispatch: record an unsent notification event: "+err.Error(),
			zap.String("run_id", r.ID))
	}
	return err
}

// announceOwedHold records the hold of a run still held: the run's own hold, or for a workflow
// parked partway through, the hold of each approval step it waits at.
func (d *Dispatcher) announceOwedHold(ctx context.Context, r *run.Run) error {
	if r.Kind != run.KindPipeline {
		return d.recordNamed(r, named.Branch{})
	}
	started, err := run.Started(ctx, d.store, r.ID)
	if err != nil {
		return err
	}
	if !started {
		return d.recordNamed(r, named.Branch{})
	}
	waiting, err := run.PendingApprovalSteps(ctx, d.store, r.ID)
	if err != nil {
		return err
	}
	for _, node := range waiting {
		if err := d.recordNamed(d.stepHold(r, node)); err != nil {
			return err
		}
	}
	return nil
}

// unsentReason says why a start or hold the run moved past was not sent, naming where the run had
// gone.
func unsentReason(event string, status run.Status) string {
	what := "start"
	if event == run.OwedHold {
		what = "hold"
	}
	if status.Terminal() {
		return "not sent: the run had already ended, " + string(status) + ", before its " + what +
			" could be announced"
	}
	return "not sent: the run had already moved on to " + string(status) + " before its " + what +
		" could be announced"
}
