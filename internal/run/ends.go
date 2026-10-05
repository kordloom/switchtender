package run

import (
	"context"
	"time"
)

// EndLedger is a store that marks a top-level run's end owed to its notification targets in the
// same write that moves the run to a terminal status, whatever path makes the move, and keeps the
// mark until a process settles it once the end is recorded for those targets. A process that stops
// between ending a run and announcing it, or whose announcement the store refused, leaves the end
// owed rather than lost, and the sweep of owed ends announces it. A run stored already finished, a
// child of a split or pipeline, and a run that has not ended are never owed.
type EndLedger interface {
	// OwedEnds returns the ids of the runs whose end has been owed for at least grace, by the
	// store's own clock, oldest first, at most limit of them when limit is above zero.
	OwedEnds(ctx context.Context, grace time.Duration, limit int) ([]string, error)
	// SettleEnd marks a run's end announced. Settling an end that is not owed changes nothing.
	SettleEnd(ctx context.Context, id string) error
}
