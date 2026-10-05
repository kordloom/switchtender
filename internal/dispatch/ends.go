package dispatch

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	named "github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/run"
)

// Owed run end sweep settings.
const (
	// endSweepInterval is how often a process looks for run ends nobody recorded for their named
	// targets.
	endSweepInterval = time.Second
	// endGrace is how long a run's end is left to the process that ended it before the sweep
	// records it instead. That process commits the run's outcome to the chain before it announces
	// the end, and the grace lets it, so the announcement normally follows the chain entry.
	endGrace = 2 * time.Second
	// endSweepBatch bounds how many owed ends one sweep records.
	endSweepBatch = 100
)

// announceEnd tells a top-level run's named targets that it ended and then settles the end in the
// store's ledger, so the sweep of owed ends leaves it alone. An end the outbox could not record is
// left owed, and the sweep records it once the store answers again. Without an outbox the targets
// are told directly, best effort, and there is nothing durable to wait for.
func (d *Dispatcher) announceEnd(r *run.Run) {
	if d.outbox != nil && r.ID != "" {
		if d.recordNamed(r, named.Branch{}) != nil {
			return
		}
	} else {
		d.notifyNamed(r)
	}
	d.settleEnd(r.ID)
}

// settleEnd marks a run's end announced in the store's ledger of owed ends, when the store keeps
// one. A settle that fails leaves the end owed, and the sweep records it again, which the outbox
// keeps to one end per run.
func (d *Dispatcher) settleEnd(id string) {
	ledger, ok := d.store.(run.EndLedger)
	if !ok || id == "" {
		return
	}
	if err := withRetries(func() error {
		return ledger.SettleEnd(context.Background(), id)
	}); err != nil {
		d.log.Warn("dispatch: mark a run's end announced: "+err.Error(), zap.String("run_id", id))
	}
}

// watchEnds sweeps the owed run ends until the dispatcher closes.
func (d *Dispatcher) watchEnds(ledger run.EndLedger) {
	defer d.notifyWG.Done()
	ticker := time.NewTicker(endSweepInterval)
	defer ticker.Stop()
	for {
		d.sweepOwedEnds(d.ctx, ledger)
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sweepOwedEnds records, for its named targets, the end of every top-level run the store still
// holds owed past the grace, and settles each one it recorded. These are the runs whose process
// stopped between the terminal write and the announcement, whose announcement the store refused,
// or whose end no announcing path reached. A run gone from the store, purged by retention, has
// nothing left to announce and is settled. Only the named targets' durable delivery is made here:
// the direct channels are best effort by design and have no record to say whether they heard.
func (d *Dispatcher) sweepOwedEnds(ctx context.Context, ledger run.EndLedger) {
	ids, err := ledger.OwedEnds(ctx, endGrace, endSweepBatch)
	if err != nil {
		if ctx.Err() == nil {
			d.log.Error("dispatch: list owed run ends: " + err.Error())
		}
		return
	}
	for _, id := range ids {
		r, err := d.store.Get(ctx, id)
		switch {
		case errors.Is(err, run.ErrNotFound):
			// Purged by retention, so there is nothing left to announce.
		case err != nil:
			if ctx.Err() == nil {
				d.log.Error("dispatch: read a run whose end is owed: "+err.Error(),
					zap.String("run_id", id))
			}
			continue
		case r.ParentID == nil && r.Status.Terminal():
			if d.recordNamed(r, named.Branch{}) != nil {
				continue
			}
		}
		if err := ledger.SettleEnd(ctx, id); err != nil && ctx.Err() == nil {
			d.log.Warn("dispatch: mark a run's end announced: "+err.Error(),
				zap.String("run_id", id))
		}
	}
}
