package dispatch

import (
	"context"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// commitOutcome records a finished run's outcome to the tamper-evident chain through the shared
// outcome package, so an in-process run and a relay run commit the same entry. It is a no-op when no
// audit chain is configured and for a child of a split or pipeline, whose outcome is rolled into its
// parent's. The append is not fail-closed: the run has already happened, so a chain that cannot
// record it is logged loudly rather than pretended away.
func (d *Dispatcher) commitOutcome(r *run.Run) {
	if d.audits == nil || r.ParentID != nil {
		return
	}
	if err := outcome.Commit(context.Background(), d.audits, d.store, r, "system:dispatcher", d.now); err != nil {
		d.log.Error("dispatch: commit run outcome: "+err.Error(), zap.String("run_id", r.ID))
	}
}
