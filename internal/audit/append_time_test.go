package audit

import (
	"testing"
	"time"
)

// TestStampAppendTimeCannotInvertAgainstThePriorEntry pins the property the whole
// zero-time-means-now contract exists to hold: a recorded time never precedes the entry before it.
//
// The handler used to read the clock and the store then took its lock and assigned the sequence,
// so two requests could be stamped in one order and sequenced in the other, leaving a trail whose
// times ran backwards on an install where no clock moved and nothing was tampered with. The chain
// still recomputed, so it never read as broken; it read as edited. Stamping under the append lock,
// pinned forward against the prior entry, is what closes that.
func TestStampAppendTimeCannotInvertAgainstThePriorEntry(t *testing.T) {
	t.Parallel()
	prev := &Entry{At: time.Unix(1000, 0)}

	// A zero-time entry is stamped now, and now is behind the prior entry's recorded time: it is
	// pinned to the prior time rather than allowed to invert.
	behind := &Entry{}
	StampAppendTime(prev, behind, time.Unix(900, 0))
	if behind.At.Before(prev.At) {
		t.Errorf("stamped %s, before the prior entry's %s: the trail would read as edited",
			behind.At, prev.At)
	}

	// A zero-time entry stamped at a later instant keeps that instant.
	ahead := &Entry{}
	StampAppendTime(prev, ahead, time.Unix(1100, 0))
	if !ahead.At.Equal(time.Unix(1100, 0)) {
		t.Errorf("stamped %s, want the append instant 1100: a forward time must be kept", ahead.At)
	}

	// A caller that chose its own time keeps it, whatever it is: the demo backdates a whole seeded
	// history on purpose, and a span beat's time is a signed claim about when the clock was read.
	chosen := &Entry{At: time.Unix(500, 0)}
	StampAppendTime(prev, chosen, time.Unix(2000, 0))
	if !chosen.At.Equal(time.Unix(500, 0)) {
		t.Errorf("a chosen time was overwritten to %s, want it kept at 500", chosen.At)
	}
}
