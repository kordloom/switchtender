package run

import "errors"

var (
	// ErrNotFound is returned when a run does not exist in the store.
	ErrNotFound = errors.New("run not found")
	// ErrNonePending is returned by Claim when no run is waiting for an executor.
	ErrNonePending = errors.New("no pending runs")
)
