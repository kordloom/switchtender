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
	// ErrCallbackPending is returned by Save when a provisioning callback run would be a second
	// unfinished one for the same template and host. See LiveCallback.
	ErrCallbackPending = errors.New("a callback run for this host is already pending")
	// ErrPartlyDelivered marks a write that landed in part before failing, so a caller must not repeat
	// the whole of it: repeating would record again what already arrived. A relay worker sends a long
	// append in batches, and a retry that started over from the first batch duplicated every batch that
	// had already landed, so the run's event record showed the same tasks executing twice.
	ErrPartlyDelivered = errors.New("write landed in part, so repeating it would duplicate")
	// ErrNotTerminal is returned when a write that settles a run is given a status that does not end
	// it, which is a caller's mistake rather than a state of the store.
	ErrNotTerminal = errors.New("status is not terminal")
	// ErrNoDecision is returned when a decision is claimed without the id that names it or the
	// claim that says how it settles, which is a caller's mistake rather than a state of the store.
	ErrNoDecision = errors.New("decision claim needs its id and its settlement")
	// ErrBadSettle is returned when a decision would settle its run into a status a decision cannot
	// reach, or into running with no owner to hold the lease.
	ErrBadSettle = errors.New("a decision settles a run into pending, running, or a terminal status")
	// ErrLimitWidens is returned when a launch limit would widen a template's target rather than
	// narrow it.
	ErrLimitWidens = errors.New("limit would widen the template's target")
	// ErrKeyTooLong is returned when a caller's idempotency key is longer than MaxClientKeyBytes.
	ErrKeyTooLong = errors.New("idempotency key too long")
	// ErrQueueTooLong is returned when a queue name is longer than MaxQueueBytes.
	ErrQueueTooLong = errors.New("queue name too long")
	// ErrNoDriftCheck is returned when a plan is kept for a run that is not a drift check still
	// running, the only run whose plan a reconcile carries.
	ErrNoDriftCheck = errors.New("run is not a running drift check, so it keeps no plan")
	// errScanLimit reports that reading one more file would pass what one playbook grade reads.
	errScanLimit = errors.New("scan limit reached")
	// errScanTooLarge reports a file larger than a playbook grade reads.
	errScanTooLarge = errors.New("file too large to scan")
)
