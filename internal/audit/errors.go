package audit

import (
	"errors"
	"fmt"
	"time"
)

// ErrExport means a bundle could not be assembled from the chain.
//
// It is named for the operation rather than the artifact: the bundle builder is the only producer
// of it now that the legacy signed export is gone.
var ErrExport = errors.New("audit export")

// ErrReservedSpan is returned by Append when an entry carries the span beat marker. The marker is
// how every reader of the chain recognizes a beat, so only AppendSpanBeat may write it; an entry
// that merely arrived wearing it is a forgery attempt, not a beat.
var ErrReservedSpan = errors.New("span marker reserved for AppendSpanBeat")

// ErrDuplicateID is returned by Append when the chain already holds an entry with the id the new
// one carries, and nothing is written. An entry's id names one event, so a second append under it
// is that event recorded again: a decision finished by two processes, the one that made it and a
// janitor after a crash, is appended once and the second finisher learns it is already there.
var ErrDuplicateID = errors.New("the chain already holds an entry with this id")

// ErrClockBehind is returned by AppendSpanBeat when the supplied time does not strictly advance
// past the newest beat already in the chain, and nothing is written. A beat's time is a signed
// claim about when the count was taken, so recording a time the clock did not read would put a
// false statement in an attestation. The honest record is a skipped beat, which a verifier reports
// as a gap with its bounds and duration rather than failing.
var ErrClockBehind = errors.New("clock behind the last span beat")

// ErrOutcomeRecorded is returned by Append for a run's outcome entry when the chain already holds
// an outcome for that run, and nothing is written. A run has exactly one outcome entry, so a
// committer that retries an append whose result it never learned, or two committers that both found
// the outcome owed, cannot put a second one on the chain. A committer reads it as success: the
// outcome is on the chain.
var ErrOutcomeRecorded = errors.New("the chain already holds this run's outcome")

// ClockBehindError is the refusal AppendSpanBeat returns when the clock has not passed the newest
// beat in the chain, or would put the beat behind the chain's newest entry. It unwraps to
// ErrClockBehind and carries both times and the beat number, so a caller reports how far behind the
// clock is without reading the chain again.
type ClockBehindError struct {
	// Prev is the recorded time the beat had to pass: the newest beat's, or the newest entry's when
	// the beat would otherwise be written behind it.
	Prev time.Time
	// At is the time the caller supplied for the beat that was refused.
	At time.Time
	// Beat is the number the refused beat would have carried. The number is not consumed, so the
	// next beat this chain accepts takes it and the numbering stays contiguous.
	Beat int64
}

// Error names the beat that was not written and both times it was decided from.
func (e *ClockBehindError) Error() string {
	return fmt.Sprintf("%s: beat %d was not written because the chain is at %s and the clock "+
		"reads %s", ErrClockBehind, e.Beat, e.Prev.UTC().Format(time.RFC3339Nano),
		e.At.UTC().Format(time.RFC3339Nano))
}

// Unwrap returns ErrClockBehind so a caller matches the sentinel with errors.Is.
func (e *ClockBehindError) Unwrap() error { return ErrClockBehind }

// ClockBehind reports the refused beat number, the last beat's recorded time, and the time the
// clock read, satisfying the interface a beat emitter detects without importing this package.
func (e *ClockBehindError) ClockBehind() (beat int64, last, clock time.Time) {
	return e.Beat, e.Prev, e.At
}

// Behind reports how far the supplied time trails the last beat.
func (e *ClockBehindError) Behind() time.Duration { return e.Prev.Sub(e.At) }

// ErrRedactParse means a body handed to CanonicalRedacted is not JSON, so there was no tree for
// the redaction to pass over and nothing can attest that the bytes are safe to disclose.
var ErrRedactParse = errors.New("redaction cannot parse this body")

// ErrRedactEncode means a body was parsed and redacted but the redacted tree would not re-encode.
// It is distinct from ErrRedactParse because the difference decides what a digest may commit to:
// a tree that was redacted and will not encode must never fall back to the bytes it came from.
var ErrRedactEncode = errors.New("redacted body will not re-encode")
