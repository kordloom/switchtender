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

	// A caller-chosen time is kept only while it does not invert the chain. Two servers on one
	// chain stamp their own wall clocks, and a decision from a fast replica followed by an
	// outcome from a slow one recorded an approval postdating the run it released, which
	// verification reports as a bypassed gate, permanently. So a chosen time behind the head is
	// pinned forward; a chosen time ahead is kept, which is all the demo's ascending backdated
	// seed ever needs, and the span beat, whose time is a signed claim that must never be
	// adjusted, refuses behind-clock appends before this function is reached.
	chosenBehind := &Entry{At: time.Unix(500, 0)}
	StampAppendTime(prev, chosenBehind, time.Unix(2000, 0))
	if !chosenBehind.At.Equal(prev.At) {
		t.Errorf("a chosen time behind the head stayed %s, want it pinned to the head's %s so "+
			"replica skew cannot record an approval after the run it released", chosenBehind.At, prev.At)
	}
	chosenAhead := &Entry{At: time.Unix(1500, 0)}
	StampAppendTime(prev, chosenAhead, time.Unix(2000, 0))
	if !chosenAhead.At.Equal(time.Unix(1500, 0)) {
		t.Errorf("a chosen time ahead of the head was overwritten to %s, want it kept", chosenAhead.At)
	}
}
