package run

import "errors"

var (
	// ErrNotFound is returned when a run does not exist in the store.
	ErrNotFound = errors.New("run not found")
	// ErrNonePending is returned by Claim when no run is waiting for an executor.
	ErrNonePending = errors.New("no pending runs")
	// ErrDuplicateKey is returned by Save when a different run tries to claim an idempotency key that
	// another run already holds. It is the store's signal that a concurrent submission won the key,
	// so the caller fetches and returns that winner instead.
	ErrDuplicateKey = errors.New("idempotency key already used")
)
