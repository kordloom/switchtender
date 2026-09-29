package audit

import (
	"errors"
	"fmt"
	"math"
	"testing"
	"time"
)

// TestSpanPathRoundTrip pins the encoding both ways: what SpanPath writes, ParseSpanPath reads
// back exactly, and near-misses are refused so they stay ordinary entries.
func TestSpanPathRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Path   string
		WantOK bool
		Beat   int64
		Count  int64
		Cad    int
	}{{ // Test 0: A written path parses back to its inputs.
		Path: SpanPath(1441, 17, 60), WantOK: true, Beat: 1441, Count: 17, Cad: 60,
	}, { // Test 1: Beat one with a large adoption count.
		Path: SpanPath(1, 18226, 300), WantOK: true, Beat: 1, Count: 18226, Cad: 300,
	}, { // Test 2: Zero count is a quiet window and valid.
		Path: SpanPath(2, 0, 60), WantOK: true, Beat: 2, Count: 0, Cad: 60,
	}, { // Test 3: Beat zero is invalid, beats start at one.
		Path: "/span/0?count=1&cadence_s=60", WantOK: false,
	}, { // Test 4: Negative count refused.
		Path: "/span/3?count=-1&cadence_s=60", WantOK: false,
	}, { // Test 5: Zero cadence refused.
		Path: "/span/3?count=1&cadence_s=0", WantOK: false,
	}, { // Test 6: Reordered parameters do not round-trip and are refused.
		Path: "/span/3?cadence_s=60&count=1", WantOK: false,
	}, { // Test 7: Trailing junk refused.
		Path: "/span/3?count=1&cadence_s=60&x=1", WantOK: false,
	}, { // Test 8: An ordinary mutation path is not a span.
		Path: "/v1/runs", WantOK: false,
	}, { // Test 9: Leading zeros do not round-trip and are refused.
		Path: "/span/03?count=1&cadence_s=60", WantOK: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			beat, count, cad, ok := ParseSpanPath(test.Path)
			if ok != test.WantOK {
				t.Fatalf("ParseSpanPath(%q) ok = %v, want %v", test.Path, ok, test.WantOK)
			}
			if ok && (beat != test.Beat || count != test.Count || cad != test.Cad) {
				t.Errorf("ParseSpanPath(%q) = %d %d %d, want %d %d %d",
					test.Path, beat, count, cad, test.Beat, test.Count, test.Cad)
			}
		})
	}
}

// TestCheckBeatAdvance pins the rule a bundle's verifiability rests on: a beat is written only when
// its time strictly leads the beat before it, and a clock that has not got there is refused rather
// than rounded up into a time it never read.
func TestCheckBeatAdvance(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		At         time.Time
		Prev       time.Time
		WantBehind time.Duration
		Want       error
	}{{ // Test 0: No beat yet, so there is nothing to advance past.
		At: base, Prev: time.Time{}, Want: nil,
	}, { // Test 1: A clock that advanced is accepted.
		At: base.Add(time.Minute), Prev: base, Want: nil,
	}, { // Test 2: A clock that stepped backward is refused, and the delta is reported.
		At: base.Add(-time.Hour), Prev: base, Want: ErrClockBehind, WantBehind: time.Hour,
	}, { // Test 3: An equal time does not advance, which is what a verifier rejects.
		At: base, Prev: base, Want: ErrClockBehind, WantBehind: 0,
	}, { // Test 4: A lead of nanoseconds alone is stored as the same instant, so it is refused.
		At: base.Add(500), Prev: base, Want: ErrClockBehind, WantBehind: 0,
	}, { // Test 5: The smallest real advance is accepted.
		At: base.Add(time.Microsecond), Prev: base, Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := CheckBeatAdvance(test.At, test.Prev, 7)
			if !errors.Is(err, test.Want) {
				t.Fatalf("CheckBeatAdvance(%s, %s) error = %v, want %v",
					test.At, test.Prev, err, test.Want)
			}
			if test.Want == nil {
				return
			}
			var behind *ClockBehindError
			if !errors.As(err, &behind) {
				t.Fatalf("CheckBeatAdvance(%s, %s) error = %v, want a *ClockBehindError carrying "+
					"the beat and both times", test.At, test.Prev, err)
			}
			if behind.Beat != 7 || behind.Behind() != test.WantBehind {
				t.Errorf("refusal beat = %d behind = %s, want 7 %s",
					behind.Beat, behind.Behind(), test.WantBehind)
			}
		})
	}
}

// TestSpanScanLimit pins that the row budget a store reads always leaves room for the beats asked
// for plus the near-miss rows that are skipped without using a slot, and that a huge limit does not
// wrap around into a budget smaller than the request.
func TestSpanScanLimit(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{-1, 0, 1, 10, 1000, 10000, math.MaxInt, math.MaxInt / 2} {
		if got := SpanScanLimit(limit); got < limit || got < 1 {
			t.Errorf("SpanScanLimit(%d) = %d, want at least the limit and at least one", limit, got)
		}
	}
}

// FuzzParseSpanPath asserts the invariant that anything accepted re-encodes to itself, so no
// two distinct paths ever parse to the same beat payload.
func FuzzParseSpanPath(f *testing.F) {
	f.Add("/span/1?count=0&cadence_s=60")
	f.Add("/span/9999999?count=123456&cadence_s=1")
	f.Add("/v1/templates")
	f.Fuzz(func(t *testing.T, path string) {
		beat, count, cad, ok := ParseSpanPath(path)
		if ok && SpanPath(beat, count, cad) != path {
			t.Errorf("accepted %q but it re-encodes to %q", path, SpanPath(beat, count, cad))
		}
	})
}
