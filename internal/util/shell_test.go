package util

import (
	"fmt"
	"testing"
)

// TestShellArgQuotesOnlyWhatNeedsIt covers the argument a printed command carries: an ordinary path
// stays readable, and anything a shell would split or interpret is quoted into one word.
func TestShellArgQuotesOnlyWhatNeedsIt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In, Want string
	}{
		{In: "/data/switchtender.db", Want: "/data/switchtender.db"}, // Test 0.
		{In: "/Users/a b/st.db", Want: "'/Users/a b/st.db'"},         // Test 1.
		{In: "it's.db", Want: `'it'\''s.db'`},                        // Test 2.
		{In: "$HOME/x.db", Want: "'$HOME/x.db'"},                     // Test 3.
		{In: "", Want: "''"},                                         // Test 4.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := ShellArg(test.In); got != test.Want {
				t.Errorf("ShellArg(%q) = %q, want %q", test.In, got, test.Want)
			}
		})
	}
}

// TestShellQuoteSurvivesItsOwnQuote pins the quoting rule directly, including the single quote a
// naive wrapper would let out. A path that closes its own quoting turns the rest of the line into
// commands the export chose.
func TestShellQuoteSurvivesItsOwnQuote(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In         string
		WantResult string
	}{
		{In: "/opt/run.sh", WantResult: `'/opt/run.sh'`},             // Test 0.
		{In: "", WantResult: `''`},                                   // Test 1.
		{In: "with space", WantResult: `'with space'`},               // Test 2.
		{In: `it's`, WantResult: `'it'\''s'`},                        // Test 3.
		{In: `'; rm -rf /; '`, WantResult: `''\''; rm -rf /; '\'''`}, // Test 4.
		{In: `$(id)`, WantResult: `'$(id)'`},                         // Test 5.
		{In: "back`tick`", WantResult: "'back`tick`'"},               // Test 6.
		{In: "生产/run.sh", WantResult: `'生产/run.sh'`},                 // Test 7.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := ShellQuote(test.In); got != test.WantResult {
				t.Errorf("ShellQuote(%q) = %q, want %q", test.In, got, test.WantResult)
			}
		})
	}
}
