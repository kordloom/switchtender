package handoff

import "errors"

var (
	// ErrKey is returned when a delivery key, public or private, cannot be parsed or used.
	ErrKey = errors.New("invalid delivery key")
	// ErrKeyFile is returned when a private delivery key file cannot be trusted: it is missing,
	// unreadable, or readable by another account.
	ErrKeyFile = errors.New("delivery key file refused")
	// ErrUnknownKey is returned when a delivery was sealed to a key the opening worker does not hold.
	ErrUnknownKey = errors.New("delivery sealed to a key this worker does not hold")
	// ErrEnvelope is returned when a sealed delivery is malformed or names a format this build does
	// not open.
	ErrEnvelope = errors.New("malformed sealed delivery")
	// ErrBinding is returned when a sealed delivery does not open for the run, lease, and worker it
	// is presented for: it was sealed for another claim, or it was altered on the way.
	ErrBinding = errors.New("sealed delivery does not open for this claim")
	// ErrSeal is returned when a payload cannot be sealed.
	ErrSeal = errors.New("seal delivery")
)
