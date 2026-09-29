package util

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestPyQuoteRoundTrips pins that every string PyQuote writes, PyUnquote reads back unchanged, over
// the characters an inventory value is likely to hold and the ones that would break a line.
func TestPyQuoteRoundTrips(t *testing.T) {
	t.Parallel()
	for testNum, in := range []string{
		"", "plain", "1.10", "True", "two words", `it's`, `say "hi"`, `back\slash`, "a#b",
		"line\nbreak", "tab\there", "\x00\x1f\x7f", "\u0085  ", "生产", `\x41 not an escape`,
	} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			quoted := PyQuote(in)
			got, ok := PyUnquote(quoted)
			if !ok {
				t.Fatalf("PyUnquote(%s) did not read the literal PyQuote wrote", quoted)
			}
			if diff := cmp.Diff(in, got); diff != "" {
				t.Errorf("round trip through %s mismatch (-want +got):\n%s", quoted, diff)
			}
		})
	}
}

// TestPyQuote pins the literal form, the part Ansible reads.
func TestPyQuote(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantResult string
		In         string
	}{{ // Test 0: A string Python would read as a float stays a string.
		In: "1.10", WantResult: `'1.10'`,
	}, { // Test 1: A quote inside is escaped.
		In: "it's", WantResult: `'it\'s'`,
	}, { // Test 2: A line break is written as an escape, keeping the literal on one line.
		In: "a\nb", WantResult: `'a\nb'`,
	}, { // Test 3: A separator Python counts as a line break is escaped too.
		In: "a\u2028b", WantResult: `'a\u2028b'`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantResult, PyQuote(test.In)); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestPyUnquote pins the decoder on input it did not write.
func TestPyUnquote(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantResult string
		In         string
		WantOK     bool
	}{{ // Test 0: A double-quoted literal.
		In: `"s3cret"`, WantResult: "s3cret", WantOK: true,
	}, { // Test 1: Escapes in a single-quoted literal.
		In: `'p\'w\\d'`, WantResult: `p'w\d`, WantOK: true,
	}, { // Test 2: An unknown escape keeps its backslash, as Python does.
		In: `'a\qb'`, WantResult: `a\qb`, WantOK: true,
	}, { // Test 3: A hex escape cut short is kept as written.
		In: `'a\x4'`, WantResult: `a\x4`, WantOK: true,
	}, { // Test 4: Not a literal.
		In: `s3cret`,
	}, { // Test 5: Mismatched quotes.
		In: `'s3cret"`,
	}, { // Test 6: Two literals side by side are not one.
		In: `'a' 'b'`,
	}, { // Test 7: Too short.
		In: `'`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, ok := PyUnquote(test.In)
			if ok != test.WantOK {
				t.Fatalf("PyUnquote(%s) ok = %t, want %t", test.In, ok, test.WantOK)
			}
			if diff := cmp.Diff(test.WantResult, got); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
