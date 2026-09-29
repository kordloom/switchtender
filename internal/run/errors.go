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
	// ErrPartlyDelivered marks a write that landed in part before failing, so a caller must not repeat
	// the whole of it: repeating would record again what already arrived. A relay worker sends a long
	// append in batches, and a retry that started over from the first batch duplicated every batch that
	// had already landed, so the run's event record showed the same tasks executing twice.
	ErrPartlyDelivered = errors.New("write landed in part, so repeating it would duplicate")
	// ErrLimitWidens is returned when a launch limit would widen a template's target rather than
	// narrow it.
	ErrLimitWidens = errors.New("limit would widen the template's target")
	// errScanLimit reports that reading one more file would pass what one playbook grade reads.
	errScanLimit = errors.New("scan limit reached")
	// errScanTooLarge reports a file larger than a playbook grade reads.
	errScanTooLarge = errors.New("file too large to scan")
)
