package decision

import "errors"

var (
	// ErrNotFound is returned when no record has the requested id.
	ErrNotFound = errors.New("decision record not found")
	// ErrExists is returned when a record is saved under an id another record already holds. A
	// record is written once, by the decision or correction that made it, and never replaced.
	ErrExists = errors.New("decision record already exists")
	// ErrNoReason is returned when a redaction names a record that was given no reason.
	ErrNoReason = errors.New("this decision record carries no reason")
	// ErrRedacted is returned when a redaction names a reason that was already redacted.
	ErrRedacted = errors.New("this reason was already redacted")
	// ErrCommitment is returned when a commitment cannot be computed from what it was given.
	ErrCommitment = errors.New("reason commitment")
	// ErrRandom is returned when the system random source cannot supply a commitment's random value.
	ErrRandom = errors.New("reason random value unavailable")
)
