package witness

import "errors"

var (
	// ErrProofBase reports a push whose consistency proof starts from a size the witness does not
	// hold. The witness answers with the size it does hold and the install proves again from there.
	ErrProofBase = errors.New("proof base")
	// ErrStale reports a push that proves from the size the witness holds but claims a smaller
	// tree. From one push a replica's superseded beat and a chain cut back look the same, so the
	// witness answers with the size it holds and accuses nothing.
	ErrStale = errors.New("stale")
	// ErrRefused reports a push the witness would not countersign because it raised findings.
	ErrRefused = errors.New("refused")
	// ErrNotEnrolled reports a push or a read for an install the witness has not enrolled.
	ErrNotEnrolled = errors.New("not enrolled")
	// ErrLapsed reports an install whose license term and grace period have both passed.
	ErrLapsed = errors.New("lapsed")
	// ErrSignature reports a signature that does not verify over the object it claims to sign.
	ErrSignature = errors.New("signature")
	// ErrMalformed reports a protocol object with a member that is missing or out of form.
	ErrMalformed = errors.New("malformed")
	// ErrNotBorn reports an enrollment whose install id is not one its key was born to. From the
	// bytes alone such a request is a lift of another install's name or a rotated identity.
	ErrNotBorn = errors.New("install id not born to key")
	// ErrClosed reports an enrollment sent to a witness that accepts none.
	ErrClosed = errors.New("enrollments closed")
	// ErrEntitlement reports an enrollment the enrollment policy refused.
	ErrEntitlement = errors.New("not entitled")
	// ErrLabel reports a key enrolling under another label than the one it is enrolled as. One key
	// keeps one label and one memory.
	ErrLabel = errors.New("enrolled under another label")
	// ErrCap reports a license that already enrolled as many installs as one license may.
	ErrCap = errors.New("install cap")
	// ErrLimited reports a push or an enrollment past its rate.
	ErrLimited = errors.New("rate limited")
	// ErrWrite reports memory the witness could not write. Nothing is countersigned without it.
	ErrWrite = errors.New("memory write")
)
