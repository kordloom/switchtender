package dispatch

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

const (
	// owedOutcomeGrace is how long an outcome stays owed before the janitor commits it rather than
	// leaving it to the process that finished the run, which commits it moments after the terminal
	// write. The chain takes one outcome per run whoever commits it, so the grace only keeps the
	// entry naming the process that saw the run finish.
	owedOutcomeGrace = 5 * time.Second
	// owedOutcomeBatch bounds how many owed outcomes one janitor sweep commits.
	owedOutcomeBatch = 100
)

// commitOutcome records a finished run's outcome to the tamper-evident chain through the shared
// outcome package, so an in-process run and a relay run commit the same entry. It is a no-op when no
// audit chain is configured and for a child of a split or pipeline, whose outcome is rolled into its
// parent's. The append is not fail-closed: the run has already happened, so a chain that cannot
// record it is logged loudly rather than pretended away, and the store keeps the outcome owed until
// the janitor commits it.
func (d *Dispatcher) commitOutcome(r *run.Run) {
	if d.audits == nil || r.ParentID != nil {
		return
	}
	if err := outcome.CommitOwed(context.Background(), d.audits, d.store, r, "system:dispatcher",
		d.now); err != nil {
		d.log.Error("dispatch: commit run outcome, the janitor commits it once the chain takes it: "+
			err.Error(), zap.String("run_id", r.ID))
	}
}

// commitOwed commits the outcome of every finished run the store says still owes one: a run whose
// append the chain refused, whose process died between the terminal write and the append, or that
// some other process settled. The page's promise is that every process that finishes a run commits
// its outcome, and this is how that holds for the runs nobody is left to finish.
func (d *Dispatcher) commitOwed() {
	if d.audits == nil {
		return
	}
	ids, err := d.store.OwedOutcomes(d.ctx, owedOutcomeGrace, owedOutcomeBatch)
	if err != nil {
		if d.ctx.Err() == nil {
			d.log.Error("dispatch: list owed outcomes: " + err.Error())
		}
		return
	}
	for _, id := range ids {
		r, err := d.store.Get(d.ctx, id)
		if err == nil {
			err = outcome.CommitOwed(d.ctx, d.audits, d.store, r, "system:janitor", d.now)
		}
		if err != nil && d.ctx.Err() == nil {
			d.log.Error("dispatch: commit owed outcome: "+err.Error(), zap.String("run_id", id))
		}
	}
}
