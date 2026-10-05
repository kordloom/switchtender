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

// The events a top-level run owes its notification targets before it ends, named as the
// notification events they are announced as.
const (
	// OwedStart is a run that began running: moved into running from any other status.
	OwedStart = "started"
	// OwedHold is a run held for a person: stored held, or moved into pending_approval or parked
	// from a status that was not already held.
	OwedHold = "approval"
)

// OwedEvent is a start or a hold of a top-level run still owed to its notification targets.
type OwedEvent struct {
	// RunID is the run.
	RunID string
	// Event is OwedStart or OwedHold.
	Event string
}

// EventLedger is a store that marks a top-level run's start and its hold owed to its notification
// targets in the same write that moves the run, the way EndLedger marks its end, and keeps each
// mark until a process settles it. A start is owed when a run moves into running from any other
// status: a claim, a coordinator's start, a decision that starts it, or a workflow resumed at an
// approval step. A hold is owed when a run is stored held, or moves into pending_approval or parked
// from a status that was not already held, so a decision that claims a hold and lets it go again
// owes nothing new. A child of a split or pipeline, and a run stored already running, owe nothing.
// A start or hold owed while one is already owed for the same run is the same mark.
type EventLedger interface {
	// OwedEvents returns the starts and holds owed for at least grace, by the store's own clock,
	// oldest first, at most limit of them when limit is above zero.
	OwedEvents(ctx context.Context, grace time.Duration, limit int) ([]OwedEvent, error)
	// SettleEvent marks a run's start or hold announced. Settling one that is not owed changes
	// nothing.
	SettleEvent(ctx context.Context, id, event string) error
}
