package inventory

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestPyShlexSplit pins how an INI host line splits into words: Python's shlex.split in POSIX mode
// with comments on, which Ansible uses. Every expected split was taken from Python itself.
func TestPyShlexSplit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Want       error
		Line       string
		WantTokens []string
	}{{ // Test 0: Spaces and tabs separate words.
		Line: "web01 a=1\tb=2", WantTokens: []string{"web01", "a=1", "b=2"},
	}, { // Test 1: Quotes group and are removed.
		Line: `h x="two words" y='single'`, WantTokens: []string{"h", "x=two words", "y=single"},
	}, { // Test 2: Quoted parts join the word around them.
		Line: `a"b c"d`, WantTokens: []string{"ab cd"},
	}, { // Test 3: A # between words ends the line.
		Line: "h a=1 # b=2", WantTokens: []string{"h", "a=1"},
	}, { // Test 4: A # inside a word ends the line too, keeping the word so far.
		Line: "h a=1#b c=2", WantTokens: []string{"h", "a=1"},
	}, { // Test 5: A # inside quotes is text.
		Line: `h a="1#b"`, WantTokens: []string{"h", "a=1#b"},
	}, { // Test 6: A backslash escapes outside quotes.
		Line: `h a=x\ y`, WantTokens: []string{"h", "a=x y"},
	}, { // Test 7: Inside double quotes it escapes only a quote or a backslash.
		Line: `h a="x\"y\d"`, WantTokens: []string{"h", `a=x"y\d`},
	}, { // Test 8: Inside single quotes nothing is escaped.
		Line: `h a='x\y'`, WantTokens: []string{"h", `a=x\y`},
	}, { // Test 9: Empty quotes are an empty word.
		Line: `h a=""`, WantTokens: []string{"h", "a="},
	}, { // Test 10: An unclosed quote fails.
		Line: `h a="x`, Want: errShlexQuote,
	}, { // Test 11: A trailing backslash fails.
		Line: `h a=x\`, Want: errShlexEscape,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := pyShlexSplit(test.Line)
			if !errors.Is(err, test.Want) {
				t.Fatalf("pyShlexSplit(%q) error = %v, want %v", test.Line, err, test.Want)
			}
			if diff := cmp.Diff(test.WantTokens, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("pyShlexSplit(%q) mismatch (-want +got):\n%s", test.Line, diff)
			}
		})
	}
}
