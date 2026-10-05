package cmd

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// TestVerifyNamesALegacyDecision pins that a receipt whose approval its entry committed under the
// legacy unkeyed digest form, recorded before nonces, verifies and says so: the digest confirms a
// guess of the body for anyone holding the receipt, so the reader is told which record it is.
func TestVerifyNamesALegacyDecision(t *testing.T) {
	path := receiptForDecisionUnder(t, "ops-admin", "session", "ops-admin", true)
	verifyPubkey = ""
	var buf bytes.Buffer
	c := testCommand()
	c.SetOut(&buf)
	if err := runVerify(c, []string{path}); err != nil {
		t.Fatalf("runVerify() error = %v\n%s", err, buf.String())
	}
	want := "decision_body is committed under the unkeyed digest form from before nonces, which " +
		"anyone who can guess it can confirm"
	if got := buf.String(); !strings.Contains(got, "legacy       claim ") ||
		!strings.Contains(got, want) {
		t.Errorf("verify output does not name the legacy decision:\n%s", got)
	}
}

// TestPrintableQuotesWhatWouldActOnATerminal pins that a member name or claim type, which the
// producer writes, reaches the terminal quoted whenever it carries a character that does not print.
func TestPrintableQuotesWhatWouldActOnATerminal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In         string
		WantResult string
	}{{ // Test 0: A plain name prints as it is.
		In: "decision_body", WantResult: "decision_body",
	}, { // Test 1: A non-ASCII letter prints as it is.
		In: "deciſion_body", WantResult: "deciſion_body",
	}, { // Test 2: An escape sequence is quoted, so it cannot recolor or move the terminal.
		In: "note\x1b[2J", WantResult: `"note\x1b[2J"`,
	}, { // Test 3: A newline is quoted, so it cannot forge a line of the output.
		In: "x\nVERIFIED", WantResult: `"x\nVERIFIED"`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := printable(test.In); got != test.WantResult {
				t.Errorf("printable(%q) = %q, want %q", test.In, got, test.WantResult)
			}
		})
	}
}
