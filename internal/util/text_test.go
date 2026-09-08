package util

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestSafeText pins the exact substitution a text column forces, because the alternative to
// substituting is losing the record of what a run did.
//
// PostgreSQL refuses a NUL byte and an invalid UTF-8 sequence with SQLSTATE 22021, so a terminal
// write carrying either failed, the run stayed running until the lease sweep interrupted it, and
// its real outcome and exit code were gone. SQLite accepted the same bytes, so the two backends
// disagreed about whether a run finished. Every byte a tool prints reaches this function, so the
// replacement has to be exact rather than approximate: a digest is taken over these bytes later.
func TestSafeText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the text arriving from a tool, an inventory, or a JSON body.
		In string
		// WantResult is the text a storage column can hold.
		WantResult string
	}{
		{In: "", WantResult: ""},                                 // Test 0: Nothing to clean.
		{In: "plain ascii", WantResult: "plain ascii"},           // Test 1: Ordinary text is untouched.
		{In: "héllo wörld", WantResult: "héllo wörld"},           // Test 2: Valid multibyte survives.
		{In: "日本語", WantResult: "日本語"},                           // Test 3: Non-Latin script too.
		{In: "\x00", WantResult: replacement},                    // Test 4: A bare NUL byte.
		{In: "a\x00b", WantResult: "a" + replacement + "b"},      // Test 5: A NUL inside real text.
		{In: "\xff", WantResult: replacement},                    // Test 6: A byte no rune starts with.
		{In: "\xff\xfe", WantResult: replacement},                // Test 7: A run collapses to one mark.
		{In: "\xed\xa0\x80", WantResult: replacement},            // Test 8: An encoded surrogate half.
		{In: "\xc3", WantResult: replacement},                    // Test 9: A truncated two-byte rune.
		{In: "\xf0\x9f\x98", WantResult: replacement},            // Test 10: A truncated emoji.
		{In: "a\xffb\x00c", WantResult: "a�b�c"},                 // Test 11: Both faults at once.
		{In: replacement, WantResult: replacement},               // Test 12: An already-replaced mark.
		{In: "line\nbreak\ttab", WantResult: "line\nbreak\ttab"}, // Test 13: Whitespace is content.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := SafeText(test.In)
			if diff := cmp.Diff(test.WantResult, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("SafeText() mismatch (-want +got):\n%s", diff)
			}
			if !utf8.ValidString(got) {
				t.Errorf("SafeText(%q) = %q, which PostgreSQL still refuses", test.In, got)
			}
			if strings.ContainsRune(got, 0) {
				t.Errorf("SafeText(%q) = %q, which still carries a NUL", test.In, got)
			}
		})
	}
}

// TestSafeTextOnAVeryLongValue proves the cleaner holds for a value the size of a real tool's
// output, where the only invalid byte sits at the very end. A scan that stopped early would store
// bytes a terminal write then refuses, which is how a run went missing rather than failing.
func TestSafeTextOnAVeryLongValue(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("ok ", 200000) + "\xff"
	got := SafeText(long)
	if !utf8.ValidString(got) {
		t.Fatal("SafeText left an invalid byte in a long value, so its terminal write still fails")
	}
	if !strings.HasSuffix(got, replacement) {
		t.Errorf("SafeText() tail = %q, want the trailing bad byte replaced", got[len(got)-8:])
	}
	if len(got) != len(long)-1+len(replacement) {
		t.Errorf("SafeText() length = %d, want only the one bad byte rewritten", len(got))
	}
}

// TestSafeTexts pins that a clean slice is handed back as itself and a dirty one is copied, so a
// caller's slice is never rewritten underneath it. Run.Sanitize passes a run's tags straight
// through, and mutating the caller's backing array would change a value another goroutine holds.
func TestSafeTexts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the slice handed in.
		In []string
		// WantResult is the cleaned slice.
		WantResult []string
		// WantSame is whether the identical backing array should come back.
		WantSame bool
	}{{ // Test 0: Nil stays nil and allocates nothing.
		In: nil, WantResult: nil, WantSame: true,
	}, { // Test 1: An empty slice is returned as itself.
		In: []string{}, WantResult: []string{}, WantSame: true,
	}, { // Test 2: All clean, so the same slice comes back.
		In: []string{"a", "b"}, WantResult: []string{"a", "b"}, WantSame: true,
	}, { // Test 3: The first element is dirty, so a copy is made.
		In: []string{"\xff", "b"}, WantResult: []string{replacement, "b"}, WantSame: false,
	}, { // Test 4: The last element is dirty, which the copy loop has to reach.
		In: []string{"a", "b\x00"}, WantResult: []string{"a", "b" + replacement}, WantSame: false,
	}, { // Test 5: Every element after the first dirty one is cleaned too.
		In:         []string{"a", "\xff", "\x00", "d"},
		WantResult: []string{"a", replacement, replacement, "d"}, WantSame: false,
	}, { // Test 6: A single dirty element.
		In: []string{"\xed\xa0\x80"}, WantResult: []string{replacement}, WantSame: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			original := append([]string(nil), test.In...)
			got := SafeTexts(test.In)
			if diff := cmp.Diff(test.WantResult, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("SafeTexts() mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(original, test.In, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("SafeTexts() rewrote the caller's slice (-before +after):\n%s", diff)
			}
			if len(test.In) == 0 {
				return
			}
			same := &got[0] == &test.In[0]
			if same != test.WantSame {
				t.Errorf("SafeTexts() returned the same backing array = %v, want %v",
					same, test.WantSame)
			}
		})
	}
}

// TestSafeStringMap pins the cleaning of a run's labels, which are user supplied on both sides of
// the pair. A key is as capable of carrying a stray byte as a value, and a label map is written to
// the same text columns the run is.
func TestSafeStringMap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the map handed in.
		In map[string]string
		// WantResult is the cleaned map.
		WantResult map[string]string
	}{{ // Test 0: Nil comes back as nil rather than an empty map.
		In: nil, WantResult: nil,
	}, { // Test 1: An empty map is returned untouched.
		In: map[string]string{}, WantResult: map[string]string{},
	}, { // Test 2: Clean pairs pass through.
		In:         map[string]string{"env": "prod", "ticket": "OPS-1"},
		WantResult: map[string]string{"env": "prod", "ticket": "OPS-1"},
	}, { // Test 3: A dirty value is cleaned.
		In:         map[string]string{"env": "pr\x00d"},
		WantResult: map[string]string{"env": "pr�d"},
	}, { // Test 4: A dirty key is cleaned too, since a key is a text column as well.
		In:         map[string]string{"e\xffnv": "prod"},
		WantResult: map[string]string{"e�nv": "prod"},
	}, { // Test 5: Both sides at once.
		In:         map[string]string{"\xff": "\x00"},
		WantResult: map[string]string{replacement: replacement},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := SafeStringMap(test.In)
			if diff := cmp.Diff(test.WantResult, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("SafeStringMap() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSafeStringMapCollapsesKeysThatCleanAlike records what happens when two distinct keys become
// the same key once their invalid bytes are replaced: one pair wins and the other is gone.
//
// This is the cost of replacing rather than refusing, and it is the deliberate trade the package
// documents: a visible replacement character beats a stranded run. Pinning it means a change to
// that trade is a decision somebody makes rather than something a caller discovers in production.
func TestSafeStringMapCollapsesKeysThatCleanAlike(t *testing.T) {
	t.Parallel()
	got := SafeStringMap(map[string]string{"\xffa": "first", "\xfea": "second"})
	if len(got) != 1 {
		t.Fatalf("SafeStringMap() = %q, want the two keys collapsed into one", got)
	}
	if _, ok := got[replacement+"a"]; !ok {
		t.Errorf("SafeStringMap() = %q, want the surviving key cleaned", got)
	}
}

// TestSafeAnyMap pins the cleaning of a run's extra vars and outputs, which are arbitrary JSON.
// Only text is what a column refuses, so a number, a bool, and a null have to come back exactly as
// they went in or the record of what a run was given stops matching what it was given.
func TestSafeAnyMap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the decoded JSON object.
		In map[string]any
		// WantResult is the cleaned object.
		WantResult map[string]any
	}{{ // Test 0: Nil is returned as nil.
		In: nil, WantResult: nil,
	}, { // Test 1: Empty is returned untouched.
		In: map[string]any{}, WantResult: map[string]any{},
	}, { // Test 2: Non-text values are carried through unchanged.
		In:         map[string]any{"n": float64(3), "b": true, "z": nil, "i": 7},
		WantResult: map[string]any{"n": float64(3), "b": true, "z": nil, "i": 7},
	}, { // Test 3: A string leaf is cleaned.
		In: map[string]any{"v": "a\x00b"}, WantResult: map[string]any{"v": "a�b"},
	}, { // Test 4: A key is cleaned as well.
		In: map[string]any{"k\xff": "v"}, WantResult: map[string]any{"k�": "v"},
	}, { // Test 5: A nested object is walked.
		In:         map[string]any{"o": map[string]any{"v": "\xff"}},
		WantResult: map[string]any{"o": map[string]any{"v": replacement}},
	}, { // Test 6: A slice is walked, mixed types and all.
		In:         map[string]any{"l": []any{"\x00", float64(1), nil}},
		WantResult: map[string]any{"l": []any{replacement, float64(1), nil}},
	}, { // Test 7: Objects inside slices inside objects, the shape a real extra var takes.
		In: map[string]any{"a": []any{map[string]any{"b": []any{"x\xffy"}}}},
		WantResult: map[string]any{
			"a": []any{map[string]any{"b": []any{"x�y"}}},
		},
	}, { // Test 8: An empty nested map is returned as itself, not rebuilt into nil.
		In:         map[string]any{"o": map[string]any{}},
		WantResult: map[string]any{"o": map[string]any{}},
	}, { // Test 9: An empty slice stays an empty slice.
		In: map[string]any{"l": []any{}}, WantResult: map[string]any{"l": []any{}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := SafeAnyMap(test.In)
			if diff := cmp.Diff(test.WantResult, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("SafeAnyMap() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSafeAnyMapSurvivesDeepNesting proves a deeply nested extra var does not blow the stack on the
// path every run's variables take. A JSON body arrives from a client, so its nesting depth is not
// this program's choice.
func TestSafeAnyMapSurvivesDeepNesting(t *testing.T) {
	t.Parallel()
	const depth = 2000
	var leaf any = "bad\xffbyte"
	for i := 0; i < depth; i++ {
		leaf = map[string]any{"n": []any{leaf}}
	}
	got := SafeAnyMap(map[string]any{"root": leaf})
	cur := got["root"]
	for i := 0; i < depth; i++ {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("level %d is %T, want a map", i, cur)
		}
		l, ok := m["n"].([]any)
		if !ok || len(l) != 1 {
			t.Fatalf("level %d holds %v, want a one element slice", i, m["n"])
		}
		cur = l[0]
	}
	if diff := cmp.Diff("bad"+replacement+"byte", cur); diff != "" {
		t.Errorf("the deepest leaf was not cleaned (-want +got):\n%s", diff)
	}
}
