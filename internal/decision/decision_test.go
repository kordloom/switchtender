package decision

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestCommitmentMatchesTheDocumentedConstruction pins the commitment to vectors computed outside
// Go, with Python's standard library, from the documented construction: SHA-256 over the RFC 8785
// canonical object of the event id, the random value's hex, and the reason. A third party checking
// a receipt reimplements exactly this, so a change here is a change to what every verifier has to
// do.
func TestCommitmentMatchesTheDocumentedConstruction(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Event is the decision event id.
		Event string
		// Random is the random value's hex.
		Random string
		// Text is the canonical masked reason.
		Text string
		// WantCommitment is the vector computed independently.
		WantCommitment string
	}{{ // Test 0: Plain text.
		Event: "aud_0a1b2c3d4e5f", Random: strings.Repeat("00", 32),
		Text:           "approved, change window confirmed with the database team",
		WantCommitment: "sha256:05ed4e47dca7f6e3d8011f3686ea7910ad71d758dbf4c3619ceb04e7be93d019",
	}, { // Test 1: Quotes, a line feed, a tab, and a non-ASCII letter, which JSON escapes or not.
		Event: "aud_ffeeddccbbaa", Random: strings.Repeat("ab", 32),
		Text:           "Denied: \"prod\" freeze\nline two\tand a tab, café",
		WantCommitment: "sha256:07a00ad68dd9ef3dbc0ed1644b083b0695d30716b03957a3b8ab18a766f86cb8",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := Commitment(test.Event, test.Random, test.Text)
			if err != nil {
				t.Fatalf("Commitment() error = %v", err)
			}
			if diff := cmp.Diff(test.WantCommitment, got); diff != "" {
				t.Errorf("commitment mismatch (-want +got):\n%s", diff)
			}
			if !Verify(test.WantCommitment, test.Event, test.Random, test.Text) {
				t.Error("Verify() refused the vector it was computed from")
			}
		})
	}
}

// TestACommitmentOpensOnlyForItsOwnInputs proves the binding: a commitment opens for the text, the
// random value, and the decision it was made for, and for nothing else. Moving a disclosed reason
// to another decision, editing a character, or guessing without the random value all fail.
func TestACommitmentOpensOnlyForItsOwnInputs(t *testing.T) {
	t.Parallel()
	random, commitment, err := Commit("aud_original", "ship it after the freeze")
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	other, _, err := Commit("aud_original", "ship it after the freeze")
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	tests := []struct {
		// Event, Random, and Text are what the check is given.
		Event, Random, Text string
		// WantOpen is whether the commitment opens.
		WantOpen bool
	}{{ // Test 0: Its own inputs open it.
		Event: "aud_original", Random: random, Text: "ship it after the freeze", WantOpen: true,
	}, { // Test 1: The same reason claimed for another decision does not.
		Event: "aud_another", Random: random, Text: "ship it after the freeze", WantOpen: false,
	}, { // Test 2: An edited reason does not.
		Event: "aud_original", Random: random, Text: "ship it before the freeze", WantOpen: false,
	}, { // Test 3: A guess under another random value does not.
		Event: "aud_original", Random: other, Text: "ship it after the freeze", WantOpen: false,
	}, { // Test 4: A random value that is not 32 bytes of hex does not.
		Event: "aud_original", Random: "nothex", Text: "ship it after the freeze", WantOpen: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := Verify(commitment, test.Event, test.Random, test.Text); got != test.WantOpen {
				t.Errorf("Verify() = %v, want %v", got, test.WantOpen)
			}
		})
	}
	if random == other {
		t.Error("two commitments drew the same random value, so they do not hide what they commit")
	}
}

// TestCommitmentRefusesUnusableInputs pins that a commitment is never computed over inputs that
// would not bind it.
func TestCommitmentRefusesUnusableInputs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Event and Random are the inputs.
		Event, Random string
		// Want is the error.
		Want error
	}{{ // Test 0: No event id.
		Event: "", Random: strings.Repeat("11", 32), Want: ErrCommitment,
	}, { // Test 1: A short random value.
		Event: "aud_x", Random: strings.Repeat("11", 16), Want: ErrCommitment,
	}, { // Test 2: A random value that is not hex.
		Event: "aud_x", Random: strings.Repeat("zz", 32), Want: ErrCommitment,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if _, err := Commitment(test.Event, test.Random, "text"); !errors.Is(err, test.Want) {
				t.Errorf("Commitment() error = %v, want %v", err, test.Want)
			}
		})
	}
}

// TestCanonical pins the one form a reason is masked, stored, committed, and disclosed in.
func TestCanonical(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the text as submitted.
		In string
		// WantText is its canonical form.
		WantText string
	}{{ // Test 0: Surrounding whitespace goes, inner spacing stays.
		In: "  approved,  window ok \n\n", WantText: "approved,  window ok",
	}, { // Test 1: Windows and old Mac line breaks become line feeds.
		In: "one\r\ntwo\rthree", WantText: "one\ntwo\nthree",
	}, { // Test 2: A terminal escape sequence loses its control character.
		In: "ok\x1b[2Jcleared", WantText: "ok[2Jcleared",
	}, { // Test 3: A tab stays, a NUL and a bell go.
		In: "a\tb\x00c\x07d", WantText: "a\tbcd",
	}, { // Test 4: A direction override goes, so the text displays as recorded.
		In: "approve\u202eevil", WantText: "approveevil",
	}, { // Test 5: Invalid UTF-8 becomes the replacement character.
		In: "bad\xffbyte", WantText: "bad�byte",
	}, { // Test 6: Whitespace alone is no reason at all.
		In: " \n\t ", WantText: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := Canonical(test.In)
			if diff := cmp.Diff(test.WantText, got); diff != "" {
				t.Errorf("Canonical() mismatch (-want +got):\n%s", diff)
			}
			if again := Canonical(got); again != got {
				t.Errorf("Canonical() is not idempotent: %q then %q", got, again)
			}
		})
	}
}

// TestTooLong pins the cap in characters, not bytes, so a reason in a script whose letters take
// several bytes is not cut short of what a person can type.
func TestTooLong(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Text is the canonical reason.
		Text string
		// WantTooLong is whether it is over the cap.
		WantTooLong bool
	}{{ // Test 0: Exactly the cap.
		Text: strings.Repeat("a", MaxReasonChars), WantTooLong: false,
	}, { // Test 1: One past it.
		Text: strings.Repeat("a", MaxReasonChars+1), WantTooLong: true,
	}, { // Test 2: The cap in two-byte letters is still the cap.
		Text: strings.Repeat("é", MaxReasonChars), WantTooLong: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := TooLong(test.Text); got != test.WantTooLong {
				t.Errorf("TooLong() = %v, want %v", got, test.WantTooLong)
			}
		})
	}
}

// TestReasonRequirements pins how a rule's requirement composes and what it demands.
func TestReasonRequirements(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// A and B are two requirements covering one run.
		A, B string
		// WantStricter is what the run carries.
		WantStricter string
		// WantApprove and WantDeny are whether an approval and a denial need a reason under it.
		WantApprove, WantDeny bool
	}{{ // Test 0: No rule asks.
		A: "", B: "", WantStricter: "", WantApprove: false, WantDeny: false,
	}, { // Test 1: One rule asks on denials.
		A: "", B: RequireDenials, WantStricter: RequireDenials, WantApprove: false, WantDeny: true,
	}, { // Test 2: The stricter rule wins whichever order the rules are in.
		A: RequireAlways, B: RequireDenials, WantStricter: RequireAlways, WantApprove: true,
		WantDeny: true,
	}, { // Test 3: The same, reversed.
		A: RequireDenials, B: RequireAlways, WantStricter: RequireAlways, WantApprove: true,
		WantDeny: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := Stricter(test.A, test.B)
			if diff := cmp.Diff(test.WantStricter, got); diff != "" {
				t.Errorf("Stricter() mismatch (-want +got):\n%s", diff)
			}
			if Required(got, true) != test.WantApprove || Required(got, false) != test.WantDeny {
				t.Errorf("Required(%q) = approve %v deny %v, want %v %v", got, Required(got, true),
					Required(got, false), test.WantApprove, test.WantDeny)
			}
		})
	}
	for _, v := range []string{"", RequireDenials, RequireAlways} {
		if !ValidRequirement(v) {
			t.Errorf("ValidRequirement(%q) = false, want true", v)
		}
	}
	if ValidRequirement("sometimes") {
		t.Error("ValidRequirement(sometimes) = true, want a typo refused")
	}
}

// TestEvaluate pins the separation-of-duties record a decision on an agent's run carries.
func TestEvaluate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Required, Same, and Approve are the inputs.
		Required, Same, Approve bool
		// WantSoD is the record.
		WantSoD *SeparationOfDuties
	}{{ // Test 0: An independent approval a rule required.
		Required: true, Same: false, Approve: true,
		WantSoD: &SeparationOfDuties{Required: true, Requester: "dev-lead", Decider: "ops-admin",
			Independent: true, Result: SoDSatisfied},
	}, { // Test 1: The bound account approving where no rule required otherwise.
		Required: false, Same: true, Approve: true,
		WantSoD: &SeparationOfDuties{Requester: "dev-lead", Decider: "ops-admin",
			Independent: false, Result: SoDNotRequired},
	}, { // Test 2: A denial, which separation of duties never restricts.
		Required: true, Same: true, Approve: false,
		WantSoD: &SeparationOfDuties{Required: true, Requester: "dev-lead", Decider: "ops-admin",
			Independent: false, Result: SoDNotApplicable},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := Evaluate(test.Required, "dev-lead", "ops-admin", test.Same, test.Approve)
			if diff := cmp.Diff(test.WantSoD, got); diff != "" {
				t.Errorf("Evaluate() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
