package inventory

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestPyParseValue pins the typing Ansible's INI plugin gives a value, which is Python's
// ast.literal_eval with the text kept when literal_eval raises. Every expected value here was taken
// from Python itself. A literal the native engine leaves to Ansible reports ErrNeedsAnsible.
func TestPyParseValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Want     error
		In       string
		WantJSON string
	}{{ // Test 0: An int.
		In: "5", WantJSON: `5`,
	}, { // Test 1: A signed int.
		In: "-7", WantJSON: `-7`,
	}, { // Test 2: Hex.
		In: "0x1F", WantJSON: `31`,
	}, { // Test 3: Octal.
		In: "0o17", WantJSON: `15`,
	}, { // Test 4: Binary.
		In: "0b101", WantJSON: `5`,
	}, { // Test 5: Underscores.
		In: "1_000", WantJSON: `1000`,
	}, { // Test 6: A leading zero is no int.
		In: "01", WantJSON: `"01"`,
	}, { // Test 7: Zeros alone are.
		In: "00", WantJSON: `0`,
	}, { // Test 8: A float.
		In: "1.5", WantJSON: `1.5`,
	}, { // Test 9: An exponent makes a float.
		In: "1e3", WantJSON: `1000.0`,
	}, { // Test 10: A trailing point too.
		In: "5.", WantJSON: `5.0`,
	}, { // Test 11: Python's repr past sixteen digits.
		In: "1e16", WantJSON: `1e+16`,
	}, { // Test 12: And for a small float.
		In: "1e-5", WantJSON: `1e-05`,
	}, { // Test 13: A negative zero keeps its sign.
		In: "-0.0", WantJSON: `-0.0`,
	}, { // Test 14: True.
		In: "True", WantJSON: `true`,
	}, { // Test 15: None.
		In: "None", WantJSON: `null`,
	}, { // Test 16: Lowercase true is text.
		In: "true", WantJSON: `"true"`,
	}, { // Test 17: So is yes.
		In: "yes", WantJSON: `"yes"`,
	}, { // Test 18: A quoted string.
		In: "'x y'", WantJSON: `"x y"`,
	}, { // Test 19: Adjacent strings join.
		In: "'a' 'b'", WantJSON: `"ab"`,
	}, { // Test 20: An octal escape.
		In: `'\101'`, WantJSON: `"A"`,
	}, { // Test 21: A unicode escape.
		In: `'é'`, WantJSON: `"é"`,
	}, { // Test 22: An unknown escape keeps its backslash.
		In: `'\d'`, WantJSON: `"\\d"`,
	}, { // Test 23: A raw string.
		In: `r'\d'`, WantJSON: `"\\d"`,
	}, { // Test 24: A list.
		In: "[1, 'two']", WantJSON: `[1,"two"]`,
	}, { // Test 25: A tuple becomes a list.
		In: "(1, 2)", WantJSON: `[1,2]`,
	}, { // Test 26: So does a bare tuple.
		In: "1, 2", WantJSON: `[1,2]`,
	}, { // Test 27: Of one.
		In: "1,", WantJSON: `[1]`,
	}, { // Test 28: A dict.
		In: "{'a': [True, None]}", WantJSON: `{"a":[true,null]}`,
	}, { // Test 29: An int key prints as text.
		In: "{80: 'http'}", WantJSON: `{"80":"http"}`,
	}, { // Test 30: Ellipsis is left to Ansible.
		In: "...", Want: ErrNeedsAnsible,
	}, { // Test 31: A sign over parentheses.
		In: "-(5)", WantJSON: `-5`,
	}, { // Test 32: Two signs are not a literal.
		In: "-(-5)", WantJSON: `"-(-5)"`,
	}, { // Test 33: Nor is arithmetic.
		In: "1+2", WantJSON: `"1+2"`,
	}, { // Test 34: A comment is ignored.
		In: "5 # five", WantJSON: `5`,
	}, { // Test 35: Unless the value is text.
		In: "hello # there", WantJSON: `"hello # there"`,
	}, { // Test 36: A name is text.
		In: "Zürich", WantJSON: `"Zürich"`,
	}, { // Test 37: An f-string is text.
		In: "f'x'", WantJSON: `"f'x'"`,
	}, { // Test 38: An expression is text.
		In: "1if 1 else 2", WantJSON: `"1if 1 else 2"`,
	}, { // Test 39: A syntax error is text.
		In: "'unterminated", WantJSON: `"'unterminated"`,
	}, { // Test 40: Empty is empty text.
		In: "", WantJSON: `""`,
	}, { // Test 41: Complex is left to Ansible.
		In: "1j", Want: ErrNeedsAnsible,
	}, { // Test 42: So is a set.
		In: "{1, 2}", Want: ErrNeedsAnsible,
	}, { // Test 43: And the empty set.
		In: "set()", Want: ErrNeedsAnsible,
	}, { // Test 44: And bytes.
		In: "b'x'", Want: ErrNeedsAnsible,
	}, { // Test 45: And an infinite float.
		In: "1e400", Want: ErrNeedsAnsible,
	}, { // Test 46: And a named escape.
		In: `'\N{BULLET}'`, Want: ErrNeedsAnsible,
	}, { // Test 47: And a lone surrogate.
		In: `'\ud800'`, Want: ErrNeedsAnsible,
	}, { // Test 48: And a bool key.
		In: "{True: 1}", Want: ErrNeedsAnsible,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := pyParseValue(test.In)
			if !errors.Is(err, test.Want) {
				t.Fatalf("pyParseValue(%q) error = %v, want %v", test.In, err, test.Want)
			}
			if err != nil {
				return
			}
			v, err := listingValue(got)
			if err != nil {
				t.Fatalf("listingValue() error = %v", err)
			}
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(test.WantJSON, string(b)); diff != "" {
				t.Errorf("pyParseValue(%q) mismatch (-want +got):\n%s", test.In, diff)
			}
		})
	}
}

// TestPyFloatRepr pins Python's float repr: shortest digits, positional within sixteen places of
// the first digit and four after the point, exponent notation otherwise.
func TestPyFloatRepr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In       float64
		WantRepr string
	}{{ // Test 0: A whole float.
		In: 1000, WantRepr: "1000.0",
	}, { // Test 1: Shortest digits.
		In: 0.1, WantRepr: "0.1",
	}, { // Test 2: Past sixteen places.
		In: 1e16, WantRepr: "1e+16",
	}, { // Test 3: At sixteen places.
		In: 1234567890123456, WantRepr: "1234567890123456.0",
	}, { // Test 4: Four places after.
		In: 0.0001, WantRepr: "0.0001",
	}, { // Test 5: Five places after.
		In: 0.00001, WantRepr: "1e-05",
	}, { // Test 6: A mantissa with digits.
		In: 1.5e-7, WantRepr: "1.5e-07",
	}, { // Test 7: Negative.
		In: -2.5, WantRepr: "-2.5",
	}, { // Test 8: Three exponent digits.
		In: 1e100, WantRepr: "1e+100",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantRepr, pyFloatRepr(test.In)); diff != "" {
				t.Errorf("pyFloatRepr(%v) mismatch (-want +got):\n%s", test.In, diff)
			}
		})
	}
}
