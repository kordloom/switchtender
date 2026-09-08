package util

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestRedactAssignmentsShapes walks the textual forms a secret actually arrives in, because this
// one reading is what the audit digest, the inventory reader, the receipt, the dossier, the
// evidence page, and the run-log masker all share. A form it does not recognize is a secret
// disclosed on every one of those paths at once, and the reported value is what the masker later
// matches in a run's stored output, so both halves of the return matter.
func TestRedactAssignmentsShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the shape under test.
		Name string
		// In is the free text.
		In string
		// WantResult is the text with every secret value replaced by the mask.
		WantResult string
		// WantValues are the bare values reported, in order, for the masker to match literally.
		WantValues []string
	}{{ // Test 0: Nothing at all.
		Name: "empty", In: "", WantResult: "",
	}, { // Test 1: Text with no assignment in it.
		Name: "no assignment", In: "run the playbook", WantResult: "run the playbook",
	}, { // Test 2: A double quoted value keeps its quotes in the text and loses them when reported,
		// since the masker has to match the bare secret the output contains.
		Name: "double quoted", In: `ansible_password="hunter 2"`, WantResult: `ansible_password=X`,
		WantValues: []string{"hunter 2"},
	}, { // Test 3: A single quoted value behaves the same.
		Name: "single quoted", In: `ansible_password='hunter 2'`, WantResult: `ansible_password=X`,
		WantValues: []string{"hunter 2"},
	}, { // Test 4: An unquoted INI value stops at the next space, which is what the INI form allows.
		Name: "unquoted ini", In: "ansible_password=hunter2 ansible_user=deploy",
		WantResult: "ansible_password=X ansible_user=deploy", WantValues: []string{"hunter2"},
	}, { // Test 5: A YAML value runs to the end of the line, because a scalar needs no quotes to hold
		// a space and stopping at the first one left the rest of a passphrase in the clear.
		Name: "yaml runs to line end", In: "ansible_ssh_pass: two word secret\nnext: line",
		WantResult: "ansible_ssh_pass: X\nnext: line", WantValues: []string{"two word secret"},
	}, { // Test 6: Several secrets on their own lines are each replaced.
		Name: "multiline", In: "password=a\ntoken=b\nhost=web",
		WantResult: "password=X\ntoken=X\nhost=web", WantValues: []string{"a", "b"},
	}, { // Test 7: Spaces around the equals sign do not hide the assignment.
		Name: "spaced equals", In: "password  =  hunter2", WantResult: "password  =  X",
		WantValues: []string{"hunter2"},
	}, { // Test 8: A secret value that is itself empty is reported as empty, not skipped.
		Name: "empty quoted value", In: `password=""`, WantResult: "password=X",
		WantValues: []string{""},
	}, { // Test 9: A name the classifier does not know keeps its value.
		Name: "ordinary name", In: "ansible_user=deploy", WantResult: "ansible_user=deploy",
	}, { // Test 10: A secret named on a line of its own with no value at all.
		Name: "bare secret name", In: "password", WantResult: "password",
	}, { // Test 11: A unicode value survives intact into the report, so the masker matches its bytes.
		Name: "unicode value", In: "password=pässwörd", WantResult: "password=X",
		WantValues: []string{"pässwörd"},
	}, { // Test 12: A dotted name is one name, not a name and a value.
		Name: "dotted name", In: "vault.token=abc", WantResult: "vault.token=X",
		WantValues: []string{"abc"},
	}, { // Test 13: A host and port on a YAML line is not an assignment chain to rewrite.
		Name: "host port", In: "db_host: db.internal:5432", WantResult: "db_host: db.internal:5432",
	}, { // Test 14: A quoted value containing an equals sign is not re-split into another assignment.
		Name: "equals inside quotes", In: `password="a=b"`, WantResult: "password=X",
		WantValues: []string{"a=b"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got, found := RedactAssignments(test.In, "X")
			if diff := cmp.Diff(test.WantResult, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("RedactAssignments() text mismatch (-want +got):\n%s", diff)
			}
			var values []string
			for _, a := range found {
				values = append(values, a.Value)
			}
			if diff := cmp.Diff(test.WantValues, values, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("reported values mismatch (-want +got):\n%s\nthe masker matches these "+
					"literally in a run's stored output", diff)
			}
		})
	}
}

// TestRedactAssignmentsWithAnEmptyMask covers the call the run-log masker makes, which wants the
// values a text carries rather than a rewritten text. An empty mask must still remove the secret
// from the returned text, since that text is what a caller unaware of the difference would store.
func TestRedactAssignmentsWithAnEmptyMask(t *testing.T) {
	t.Parallel()
	got, found := RedactAssignments("password=hunter2 user=deploy", "")
	if diff := cmp.Diff("password= user=deploy", got); diff != "" {
		t.Errorf("RedactAssignments() mismatch (-want +got):\n%s", diff)
	}
	if len(found) != 1 || found[0].Name != "password" || found[0].Value != "hunter2" {
		t.Errorf("found = %v, want the one secret reported by name and bare value", found)
	}
}

// TestRedactAssignmentsNestingBudget pins the documented bound on how deep a secret joined onto
// another assignment's value is chased.
//
// The bound is what keeps the scan linear: without it a crafted body of joined assignments turned
// redaction quadratic on the digest path every audited change runs through. The cost of the bound
// is stated plainly in the source, that past it the remaining value is emitted unscanned, and this
// test fixes where the edge sits so a change to the budget is a decision rather than a surprise.
func TestRedactAssignmentsNestingBudget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Depth is how many ordinary assignments the secret is joined behind.
		Depth int
		// WantMasked is whether the secret is found at that depth.
		WantMasked bool
	}{
		{Depth: 0, WantMasked: true},  // Test 0: The secret is the whole text.
		{Depth: 1, WantMasked: true},  // Test 1: One level, what a real command line has.
		{Depth: 7, WantMasked: true},  // Test 2: One inside the budget.
		{Depth: 8, WantMasked: true},  // Test 3: The last level the budget covers.
		{Depth: 9, WantMasked: false}, // Test 4: One past it, emitted unscanned by design.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			in := strings.Repeat("a=", test.Depth) + "password=hunter2"
			got, _ := RedactAssignments(in, "X")
			masked := !strings.Contains(got, "hunter2")
			if masked != test.WantMasked {
				t.Errorf("at depth %d RedactAssignments(%q) = %q, masked = %v, want %v",
					test.Depth, in, got, masked, test.WantMasked)
			}
		})
	}
}

// TestRedactAssignmentsHandlesAVeryLongValue proves a single enormous secret value is masked whole
// rather than partly. A run's command line and an inventory's content are both somebody else's
// bytes, and a redaction that stopped early would leave the tail of a key in the clear.
func TestRedactAssignmentsHandlesAVeryLongValue(t *testing.T) {
	t.Parallel()
	secret := strings.Repeat("k", 500000)
	got, found := RedactAssignments("api_key="+secret, "X")
	if diff := cmp.Diff("api_key=X", got); diff != "" {
		t.Errorf("RedactAssignments() mismatch (-want +got):\n%s", diff)
	}
	if len(found) != 1 || found[0].Value != secret {
		t.Errorf("found %d assignments, want the whole value reported for the masker", len(found))
	}
}

// TestUnquote pins the stripping that lets a caller match a secret literally in a run's output.
// Keeping the quotes would mask a string the output never contains, so the masker would look for
// something that is not there and the secret would stay visible in the stored log.
func TestUnquote(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the captured value.
		In string
		// WantResult is the bare value.
		WantResult string
	}{
		{In: "", WantResult: ""},                           // Test 0: Nothing.
		{In: `"`, WantResult: `"`},                         // Test 1: One quote is not a pair.
		{In: `'`, WantResult: `'`},                         // Test 2: Same for the other kind.
		{In: `""`, WantResult: ""},                         // Test 3: An empty quoted value.
		{In: `''`, WantResult: ""},                         // Test 4: The same, single quoted.
		{In: `"abc"`, WantResult: "abc"},                   // Test 5: The ordinary case.
		{In: `'abc'`, WantResult: "abc"},                   // Test 6: And single quoted.
		{In: `"abc'`, WantResult: `"abc'`},                 // Test 7: Mismatched quotes are not a pair.
		{In: `'abc"`, WantResult: `'abc"`},                 // Test 8: Nor the other way round.
		{In: `abc`, WantResult: "abc"},                     // Test 9: Unquoted passes through.
		{In: `"a"b"`, WantResult: `a"b`},                   // Test 10: Only the outer pair goes.
		{In: `""abc""`, WantResult: `"abc"`},               // Test 11: One pair, not both.
		{In: "`abc`", WantResult: "`abc`"},                 // Test 12: A backtick is not a quote here.
		{In: `"unterminated`, WantResult: `"unterminated`}, // Test 13: An opening quote alone.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantResult, Unquote(test.In), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Unquote(%q) mismatch (-want +got):\n%s", test.In, diff)
			}
		})
	}
}

// TestClipBoundaries pins the cut a shared helper makes, since it runs over an error message, an
// operator's prompt, and a field name from a rejected request before any of those are shown or
// logged. A cut landing inside a multibyte rune would emit bytes no UTF-8 reader accepts, on the
// path where the value is already somebody else's.
func TestClipBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the value to shorten.
		In string
		// Limit is the byte budget.
		Limit int
		// WantResult is the shortened value.
		WantResult string
	}{
		{In: "", Limit: 0, WantResult: ""},           // Test 0: Empty at a zero budget.
		{In: "", Limit: 5, WantResult: ""},           // Test 1: Empty under a budget.
		{In: "abc", Limit: 3, WantResult: "abc"},     // Test 2: Exactly at the limit.
		{In: "abcd", Limit: 3, WantResult: "abc..."}, // Test 3: One byte over.
		{In: "abc", Limit: 0, WantResult: "..."},     // Test 4: A zero budget keeps nothing.
		{In: "é", Limit: 1, WantResult: "..."},       // Test 5: The cut walks out of the rune.
		{In: "aé", Limit: 2, WantResult: "a..."},     // Test 6: And back to the rune boundary.
		{In: "aé", Limit: 3, WantResult: "aé"},       // Test 7: Three bytes is the whole value.
		{In: "日本語", Limit: 4, WantResult: "日..."},    // Test 8: A three byte rune, cut mid-rune.
		{In: "日本語", Limit: 6, WantResult: "日本..."},   // Test 9: On the boundary, nothing walks.
		{In: "🔒ok", Limit: 2, WantResult: "..."},     // Test 10: A four byte rune cut early.
		{In: "ok", Limit: 100, WantResult: "ok"},     // Test 11: A budget larger than the value.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := Clip(test.In, test.Limit)
			if diff := cmp.Diff(test.WantResult, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Clip(%q, %d) mismatch (-want +got):\n%s", test.In, test.Limit, diff)
			}
		})
	}
}

// TestClipPanicsOnANegativeLimit records that the shared helper has no floor of its own.
//
// Every caller today passes a positive constant, so nothing reaches this. It is pinned because Clip
// is exported from a package whose whole purpose is one implementation for everybody: the next
// caller to compute a budget, say a cap minus a prefix already written, hands it a negative number
// and takes down whatever goroutine it is on rather than shortening a string.
func TestClipPanicsOnANegativeLimit(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Clip(%q, -1) panicked with %v, want an empty clip", "abc", r)
		}
	}()
	_ = Clip("abc", -1)
}

// TestFirstNonEmptyBoundaries pins that only a genuinely empty string is skipped. A value that is
// whitespace or a zero character is a value somebody set, and treating it as unset would silently
// substitute a fallback for a deliberate choice.
func TestFirstNonEmptyBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In are the candidates in preference order.
		In []string
		// WantResult is the value chosen.
		WantResult string
	}{
		{In: nil, WantResult: ""},                              // Test 0: Nothing given.
		{In: []string{}, WantResult: ""},                       // Test 1: An empty list.
		{In: []string{""}, WantResult: ""},                     // Test 2: One empty value.
		{In: []string{" ", "x"}, WantResult: " "},              // Test 3: A space is a value.
		{In: []string{"\x00", "x"}, WantResult: "\x00"},        // Test 4: So is a NUL.
		{In: []string{"", "", "", "last"}, WantResult: "last"}, // Test 5: The last one wins.
		{In: []string{"日", "本"}, WantResult: "日"},              // Test 6: Multibyte first.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := FirstNonEmpty(test.In...)
			if diff := cmp.Diff(test.WantResult, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("FirstNonEmpty(%q) mismatch (-want +got):\n%s", test.In, diff)
			}
		})
	}
}
