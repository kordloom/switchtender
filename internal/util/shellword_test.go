package util

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestShellWord pins how far a shell word runs and the value a shell gives it, for each piece of
// syntax a password on a command line is written with.
func TestShellWord(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the text from where the word starts.
		In string
		// Closer is the quote the line left open before the word, or zero.
		Closer byte
		// WantLen is how many bytes the word spans.
		WantLen int
		// WantValue is the value a shell gives it.
		WantValue string
	}{{ // Test 0: A bare word ends at a space.
		In: "abc def", WantLen: 3, WantValue: "abc",
	}, { // Test 1: An empty quote pair joins the text after it.
		In: "''x y", WantLen: 3, WantValue: "x",
	}, { // Test 2: The idiom for a quote inside single quotes.
		In: `'pa'\''ss' psql`, WantLen: 10, WantValue: "pa'ss",
	}, { // Test 3: An escaped space is part of the word.
		In: `x\ y z`, WantLen: 4, WantValue: "x y",
	}, { // Test 4: Runs of each quote and bare text join into one word.
		In: `"a"'b'c d`, WantLen: 7, WantValue: "abc",
	}, { // Test 5: A double-quoted run decodes only the escapes the shell honors there.
		In: `"a\"b\$c\d"`, WantLen: 11, WantValue: `a"b$c\d`,
	}, { // Test 6: A single-quoted run decodes nothing.
		In: `'a\nb'`, WantLen: 6, WantValue: `a\nb`,
	}, { // Test 7: ANSI-C quoting decodes its escapes.
		In: `$'p\x40ss\tw\'q' z`, WantLen: 16, WantValue: "p@ss\tw'q",
	}, { // Test 8: A separator ends an unquoted word.
		In: "abc;rm", WantLen: 3, WantValue: "abc",
	}, { // Test 9: A separator inside quotes does not.
		In: `"a;b"c`, WantLen: 6, WantValue: "a;bc",
	}, { // Test 10: A quote that never closes is an ordinary byte, and the word runs on.
		In: `ab"cd ef`, WantLen: 5, WantValue: `ab"cd`,
	}, { // Test 11: A quoted run may span lines, as a pasted key does.
		In: "\"one\ntwo\" x", WantLen: 9, WantValue: "one\ntwo",
	}, { // Test 12: A backslash before a newline joins two lines.
		In: "ab\\\ncd e", WantLen: 6, WantValue: "abcd",
	}, { // Test 13: Inside an enclosing double quote, its closer ends the word.
		In: `hunter2" next`, Closer: '"', WantLen: 7, WantValue: "hunter2",
	}, { // Test 14: An escaped quote does not close the enclosing string.
		In: `a\"b c"`, Closer: '"', WantLen: 4, WantValue: `a"b`,
	}, { // Test 15: An escaped quote pair groups text for the inner program.
		In: `\"a b\""`, Closer: '"', WantLen: 7, WantValue: "a b",
	}, { // Test 16: Single quotes inside an enclosing double quote still group.
		In: `'a b' deploy"`, Closer: '"', WantLen: 5, WantValue: "a b",
	}, { // Test 17: Inside an enclosing single quote a backslash is kept.
		In: `a\"b'`, Closer: '\'', WantLen: 4, WantValue: `a\"b`,
	}, { // Test 18: Double quotes inside an enclosing single quote still group.
		In: `"a b"'`, Closer: '\'', WantLen: 5, WantValue: "a b",
	}, { // Test 19: An inner run cut off by the enclosing closer is not a run.
		In: `"a'`, Closer: '\'', WantLen: 2, WantValue: `"a`,
	}, { // Test 20: A trailing backslash is kept.
		In: `ab\`, WantLen: 3, WantValue: `ab\`,
	}, { // Test 21: Nothing.
		In: "", WantLen: 0, WantValue: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var b strings.Builder
			n := shellWord(test.In, test.Closer, &b)
			if diff := cmp.Diff(test.WantLen, n); diff != "" {
				t.Errorf("shellWord(%q) length mismatch (-want +got):\n%s", test.In, diff)
			}
			if diff := cmp.Diff(test.WantValue, b.String()); diff != "" {
				t.Errorf("shellWord(%q) value mismatch (-want +got):\n%s", test.In, diff)
			}
			if got := shellWord(test.In, test.Closer, nil); got != n {
				t.Errorf("shellWord(%q) spans %d bytes without a value and %d with one", test.In, got, n)
			}
		})
	}
}

// TestOpenQuote pins which quote a line leaves open, which is how a value learns that a quote opened
// before its name holds it.
func TestOpenQuote(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the line before a value.
		In string
		// WantResult is the quote still open at its end, or zero.
		WantResult byte
	}{
		{In: "", WantResult: 0},                          // Test 0: Nothing.
		{In: `psql "host=db password=`, WantResult: '"'}, // Test 1: The ordinary shape.
		{In: `echo 'a' "b" `, WantResult: 0},             // Test 2: Closed runs.
		{In: `echo 'say "hi' x=`, WantResult: 0},         // Test 3: A double quote inside single ones.
		{In: `sh -c "echo \"hi\" x=`, WantResult: '"'},   // Test 4: Escaped quotes do not toggle.
		{In: `echo 'a\' x=`, WantResult: 0},              // Test 5: Inside single quotes a backslash is literal.
		{In: `echo \' x=`, WantResult: 0},                // Test 6: An escaped quote outside any.
		{In: `don't x=`, WantResult: '\''},               // Test 7: An apostrophe reads as an open quote.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantResult, openQuote(test.In)); diff != "" {
				t.Errorf("openQuote(%q) mismatch (-want +got):\n%s", test.In, diff)
			}
		})
	}
}

// TestYAMLValue pins how far a name: value value runs and whether its quotes stay around the mask.
func TestYAMLValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the text from where the value starts.
		In string
		// Line is the text before the value on its line.
		Line string
		// Closer is the quote the line left open, or zero.
		Closer byte
		// WantLen is how many bytes the value spans.
		WantLen int
		// WantQuotes is whether the quotes stay around the mask.
		WantQuotes bool
	}{{ // Test 0: A plain scalar runs to the end of the line.
		In: "two word secret\nnext: 1", WantLen: 15,
	}, { // Test 1: Trailing spaces are not part of it.
		In: "abc   \nnext", WantLen: 3,
	}, { // Test 2: A quoted scalar at the end of its line.
		In: `"abc"` + "\nnext", WantLen: 5,
	}, { // Test 3: A quoted scalar in a flow mapping keeps its quotes.
		In: `"hunter2","user":"bob"}`, WantLen: 9, WantQuotes: true,
	}, { // Test 4: Text joined onto a quoted scalar runs on to the end of the line.
		In: `'a'b c`, WantLen: 6,
	}, { // Test 5: A doubled single quote is one escaped quote.
		In: `'it''s'`, WantLen: 7,
	}, { // Test 6: The quote holding the text ends a plain value.
		In: `SUPERSECRET" https://api`, Closer: '"', WantLen: 11,
	}, { // Test 7: A separator ends a plain value.
		In: "abc123; deploy", WantLen: 6,
	}, { // Test 8: A block scalar takes its indented lines.
		In: "|\n  line one\n\n  line two\nnext: 1", Line: "key: ", WantLen: 24,
	}, { // Test 9: A block scalar under an indented name stops at a line no deeper than the name.
		In: "|-\n    a\n    b\n  next: 1", Line: "  key: ", WantLen: 14,
	}, { // Test 10: A bar with nothing indented under it is an empty block and assigns nothing.
		In: "|\nnext: 1", WantLen: 0,
	}, { // Test 11: An unclosed quote takes the rest of the line.
		In: `"abc;def`, WantLen: 8,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			n, quotes := yamlValue(test.In, test.Line, test.Closer)
			if diff := cmp.Diff(test.WantLen, n); diff != "" {
				t.Errorf("yamlValue(%q) length mismatch (-want +got):\n%s", test.In, diff)
			}
			if diff := cmp.Diff(test.WantQuotes, quotes); diff != "" {
				t.Errorf("yamlValue(%q) quotes mismatch (-want +got):\n%s", test.In, diff)
			}
		})
	}
}

// TestYAMLValueDecode pins the value a program receives from a name: value value as written.
func TestYAMLValueDecode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the value as written.
		In string
		// Closer is the quote the line left open, or zero.
		Closer byte
		// WantResult is what a program receives.
		WantResult string
	}{
		{In: `"a\"b\\c\u0041"`, WantResult: `a"b\cA`},          // Test 0: JSON escapes.
		{In: `'it''s'`, WantResult: "it's"},                    // Test 1: A doubled quote.
		{In: `"abc" # note`, WantResult: "abc"},                // Test 2: A comment after a quoted scalar.
		{In: "abc  # note", WantResult: "abc"},                 // Test 3: A comment after a plain one.
		{In: "abc\t# note", WantResult: "abc"},                 // Test 4: A tab before the comment.
		{In: "a#b", WantResult: "a#b"},                         // Test 5: A hash inside a word is not a comment.
		{In: `a\"b`, Closer: '"', WantResult: `a"b`},           // Test 6: Escapes of the enclosing string.
		{In: "|\n  one\n   two\n", WantResult: "one\n two\n"},  // Test 7: A block scalar, dedented.
		{In: `'a'b`, WantResult: "ab"},                         // Test 8: Joined text reads as a shell word.
		{In: `''x`, WantResult: "x"},                           // Test 9: The empty pair joined on.
		{In: `"\q"`, WantResult: `\q`},                         // Test 10: An escape JSON rejects is kept.
		{In: "two word secret", WantResult: "two word secret"}, // Test 11: A plain scalar with spaces.
		{In: `"unterminated`, WantResult: `"unterminated`},     // Test 12: An unclosed quote.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantResult, yamlValueDecode(test.In, test.Closer)); diff != "" {
				t.Errorf("yamlValueDecode(%q) mismatch (-want +got):\n%s", test.In, diff)
			}
		})
	}
}
