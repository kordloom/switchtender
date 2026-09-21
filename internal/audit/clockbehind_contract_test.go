package audit

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// beatRefusal restates the shape spanbeat detects structurally. spanbeat deliberately keeps its
// copy unexported so it imports nothing from this package, which means the compiler never checks
// the two against each other: the contract lives only in both sides spelling the same method. This
// test is where the audit side signs it.
type beatRefusal interface {
	// ClockBehind reports the refused beat number, the last beat's recorded time, and the time the
	// clock read.
	ClockBehind() (beat int64, last, clock time.Time)
}

// The compile-time half: remove or change the method and this file stops building, which is the
// loudest possible statement that spanbeat's structural detection just went blind.
var _ beatRefusal = (*ClockBehindError)(nil)

// TestClockBehindErrorSatisfiesTheBeatEmittersContract pins the duck-typed contract between this
// package's refusal and spanbeat's detection of it.
//
// The emitter finds the condition with errors.As against its own unexported interface, so nothing
// imports across the boundary and nothing compiles the agreement: renaming the method here would
// build clean, spanbeat would stop recognizing the refusal, and every clock-behind beat would be
// logged as a generic append failure with the attested-silence window quietly wrong. Coverage
// showed the method at true zero because spanbeat's own tests exercise a local double, never this
// real type, which is exactly how a broken contract would have stayed green.
func TestClockBehindErrorSatisfiesTheBeatEmittersContract(t *testing.T) {
	t.Parallel()
	refused := &ClockBehindError{
		Beat: 41, Prev: time.Unix(1000, 0), At: time.Unix(900, 0),
	}
	// Wrapped the way an append path returns it, detected the way the emitter detects it.
	err := fmt.Errorf("append beat: %w", refused)
	var behind beatRefusal
	if !errors.As(err, &behind) {
		t.Fatal("errors.As does not find the refusal through a wrap: the emitter would log a " +
			"generic failure and the attested-silence window would be wrong")
	}
	beat, last, clock := behind.ClockBehind()
	if beat != 41 || !last.Equal(time.Unix(1000, 0)) || !clock.Equal(time.Unix(900, 0)) {
		t.Fatalf("ClockBehind() = (%d, %s, %s), want the refused beat and both times intact",
			beat, last, clock)
	}
}
