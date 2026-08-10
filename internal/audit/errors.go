package audit

import (
	"errors"
	"fmt"
	"time"
)

// ErrReservedSpan is returned by Append when an entry carries the span beat marker. The marker is
// how every reader of the chain recognizes a beat, so only AppendSpanBeat may write it; an entry
// that merely arrived wearing it is a forgery attempt, not a beat.
var ErrReservedSpan = errors.New("span marker reserved for AppendSpanBeat")

// ErrClockBehind is returned by AppendSpanBeat when the supplied time does not strictly advance
// past the newest beat already in the chain, and nothing is written. A beat's time is a signed
// claim about when the count was taken, so recording a time the clock did not read would put a
// false statement in an attestation. The honest record is a skipped beat, which a verifier reports
// as a gap with its bounds and duration rather than failing.
var ErrClockBehind = errors.New("clock behind the last span beat")

// ClockBehindError is the refusal AppendSpanBeat returns when the clock has not passed the newest
// beat in the chain. It unwraps to ErrClockBehind and carries both times and the beat number, so a
// caller reports how far behind the clock is without reading the chain again.
type ClockBehindError struct {
	// Prev is the recorded time of the newest beat in the chain.
	Prev time.Time
	// At is the time the caller supplied for the beat that was refused.
	At time.Time
	// Beat is the number the refused beat would have carried. The number is not consumed, so the
	// next beat this chain accepts takes it and the numbering stays contiguous.
	Beat int64
}

// Error names the beat that was not written and both times it was decided from.
func (e *ClockBehindError) Error() string {
	return fmt.Sprintf("%s: beat %d was not written because the last beat is at %s and the clock "+
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
