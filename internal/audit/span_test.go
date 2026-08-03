package audit

import (
	"fmt"
	"testing"
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
