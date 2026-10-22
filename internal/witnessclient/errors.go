package witnessclient

import "errors"

var (
	// ErrUnreachable reports a witness that did not answer: a network error, a timeout, or a 5xx.
	// It is never a refusal. The push stays queued and is retried on the next beat.
	ErrUnreachable = errors.New("witness unreachable")
	// ErrLapsed reports a witness that answered 402: the license term and its grace have passed, so
	// the witness countersigns nothing more from this install and the client stops pushing.
	ErrLapsed = errors.New("witness lapsed")
	// ErrRefused reports a push the witness refused with findings: it contradicts what the witness
	// remembers of this install, so nothing was countersigned.
	ErrRefused = errors.New("witness refused")
	// ErrRejected reports a push the witness would not hold against its memory at all, such as one it
	// read as out of form, or an answer the client cannot read as any of the witness's answers.
	ErrRejected = errors.New("witness rejected")
	// ErrEnrollment reports an enrollment file or answer that is missing, out of form, or not this
	// install's.
	ErrEnrollment = errors.New("witness enrollment")
	// ErrWitnessKey reports a witness key this install does not hold: an answer signed by a key its
	// enrollment does not list, a pin that does not match, or a key list no held key signed.
	ErrWitnessKey = errors.New("witness key")
	// ErrChain reports a chain the client cannot build the pushed tree from: positions that are not
	// contiguous, a link that does not follow the one before it, or a beat the chain does not hold.
	ErrChain = errors.New("witness chain")
	// ErrNothingWitnessed reports a witness that holds no countersigned head for this install yet:
	// the install enrolled and no push of it was adopted. It is an answer, not an outage.
	ErrNothingWitnessed = errors.New("nothing witnessed")
)
