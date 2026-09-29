package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/audit"
)

// writeGenuineReceipt seeds a finished run, writes its receipt, and returns the receipt's path and
// the fingerprint of the key that signed it.
func writeGenuineReceipt(t *testing.T) (path, keyID string) {
	t.Helper()
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	seedReceiptableRun(t, db, "run_pinned")
	path = filepath.Join(dir, "run.receipt")
	receiptRunDB, receiptRunOut, receiptSparse = db, path, false
	t.Cleanup(func() { receiptRunDB, receiptRunOut = defaultDBPath, "" })
	if err := runReceipt(testCommand(), []string{"run_pinned"}); err != nil {
		t.Fatalf("runReceipt() error = %v", err)
	}
	signed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(receipt) error = %v", err)
	}
	rep, err := audit.VerifyBundle(signed, "")
	if err != nil {
		t.Fatalf("VerifyBundle(receipt) error = %v", err)
	}
	return path, rep.KeyID
}

// TestVerifyVerdictSaysWhetherTheSignerWasChecked pins the verdict to the check that was made.
//
// Any key signs its own receipt, so without a pin the verifier can say a receipt is intact and
// cannot say whose it is. It printed the same VERIFIED either way, so a forged receipt checked
// without --pubkey read exactly like a genuine one checked with it.
func TestVerifyVerdictSaysWhetherTheSignerWasChecked(t *testing.T) {
	receipt, keyID := writeGenuineReceipt(t)
	verdictLine := regexp.MustCompile(`(?m)^VERIFIED:`)
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Pin is the --pubkey value.
		Pin string
		// WantLines must all appear in the output.
		WantLines []string
		// DenyLines must not appear in the output.
		DenyLines []string
		// WantVerified is whether the output carries the VERIFIED verdict line.
		WantVerified bool
		// WantErr is whether verification must fail.
		WantErr bool
	}{{ // Test 0: Unpinned, the receipt is intact and its signer is unidentified.
		Name: "no pin",
		WantLines: []string{"pin          NONE", "INTACT, BUT UNIDENTIFIED",
			"/.well-known/loomseal.json"},
		DenyLines: []string{"pin          OK"},
	}, { // Test 1: Pinned to the key that signed it, the receipt is verified and says so.
		Name: "right pin", Pin: keyID,
		WantLines:    []string{"pin          OK (matches the fingerprint you pinned"},
		DenyLines:    []string{"UNIDENTIFIED", "pin          NONE"},
		WantVerified: true,
	}, { // Test 2: Pinned to a different key, the receipt is refused.
		Name: "wrong pin", Pin: "sha256:" + strings.Repeat("0", 64), WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			verifyPubkey = test.Pin
			defer func() { verifyPubkey = "" }()
			var buf bytes.Buffer
			c := testCommand()
			c.SetOut(&buf)
			err := runVerify(c, []string{receipt})
			if (err != nil) != test.WantErr {
				t.Fatalf("%s: runVerify() error = %v, want error %v\n%s", test.Name, err,
					test.WantErr, buf.String())
			}
			if test.WantErr {
				return
			}
			got := buf.String()
			for _, want := range test.WantLines {
				if !strings.Contains(got, want) {
					t.Errorf("%s: output is missing %q:\n%s", test.Name, want, got)
				}
			}
			for _, deny := range test.DenyLines {
				if strings.Contains(got, deny) {
					t.Errorf("%s: output carries %q:\n%s", test.Name, deny, got)
				}
			}
			if verdictLine.MatchString(got) != test.WantVerified {
				t.Errorf("%s: VERIFIED verdict present = %t, want %t:\n%s", test.Name,
					!test.WantVerified, test.WantVerified, got)
			}
		})
	}
}

// TestAnEmptyPinIsRefusedRatherThanDropped pins the refusal of a pin flag given nothing.
//
// A pin is usually filled in by a command substitution that fetches the published key. When that
// fetch failed the flag arrived empty, the verifier treated it as no pin, and the receipt passed
// against whatever key it named, so the one check the caller asked for silently did not happen.
func TestAnEmptyPinIsRefusedRatherThanDropped(t *testing.T) {
	receipt, _ := writeGenuineReceipt(t)
	attestation, _, _ := writeAttestation(t, nil)
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Args is the command line.
		Args []string
		// WantCode is the exit code.
		WantCode int
		// WantOut must appear on stdout, when set.
		WantOut string
	}{{ // Test 0: The control. Leaving the flag off checks integrity alone and passes.
		Name: "no flag", Args: []string{"verify", receipt}, WantCode: CodeOK,
		WantOut: "INTACT, BUT UNIDENTIFIED",
	}, { // Test 1: An empty pin, as a failed fetch leaves it, is refused.
		Name: "empty pin", Args: []string{"verify", receipt, "--pubkey", ""}, WantCode: CodeError,
	}, { // Test 2: The same with the value joined to the flag.
		Name: "empty pin joined", Args: []string{"verify", receipt, "--pubkey="}, WantCode: CodeError,
	}, { // Test 3: Whitespace is no more a key than nothing is.
		Name: "blank pin", Args: []string{"verify", receipt, "--pubkey", " \n"}, WantCode: CodeError,
	}, { // Test 4: The witness's attestation verifier had the same gap.
		Name:     "empty witness pin",
		Args:     []string{"witness", "verify-attestation", attestation, "--pubkey", ""},
		WantCode: CodeError,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: each case forks a copy of this binary.
			stdout, stderr, code := runCLI(t, test.Args...)
			if code != test.WantCode {
				t.Fatalf("%s: switchtender %q exited %d, want %d.\nstdout:\n%s\nstderr:\n%s",
					test.Name, test.Args, code, test.WantCode, stdout, stderr)
			}
			if test.WantCode != CodeOK && !strings.Contains(stderr, "empty") {
				t.Errorf("%s: the refusal does not say the pin was empty:\n%s", test.Name, stderr)
			}
			if !strings.Contains(stdout, test.WantOut) {
				t.Errorf("%s: stdout is missing %q:\n%s", test.Name, test.WantOut, stdout)
			}
		})
	}
}

// TestWitnessVerdictSaysWhetherTheSignerWasChecked pins the pinned field and the note. The JSON is
// what a script reads, and "ok" without a pin says only that the document is internally consistent,
// which an attestation a forger signed with their own key also is.
func TestWitnessVerdictSaysWhetherTheSignerWasChecked(t *testing.T) {
	good, _, keyID := writeAttestation(t, nil)
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Pin is the --pubkey value.
		Pin string
		// WantPinned is the pinned field.
		WantPinned bool
		// WantNote is whether the verdict carries the unpinned note.
		WantNote bool
	}{{ // Test 0: Unpinned, the verdict says so and says what it costs.
		Name: "no pin", WantNote: true,
	}, { // Test 1: Pinned, the verdict says so and carries no note.
		Name: "pinned", Pin: keyID, WantPinned: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			witnessVerifyPubkey = test.Pin
			defer func() { witnessVerifyPubkey = "" }()
			var buf bytes.Buffer
			c := testCommand()
			c.SetOut(&buf)
			if err := runWitnessVerify(c, []string{good}); err != nil {
				t.Fatalf("%s: runWitnessVerify() error = %v", test.Name, err)
			}
			var got struct {
				OK     bool   `json:"ok"`
				Pinned *bool  `json:"pinned"`
				Note   string `json:"note"`
			}
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatalf("%s: verdict is not JSON: %v\n%s", test.Name, err, buf.String())
			}
			if !got.OK {
				t.Errorf("%s: a sound attestation did not verify:\n%s", test.Name, buf.String())
			}
			if got.Pinned == nil || *got.Pinned != test.WantPinned {
				t.Errorf("%s: pinned = %v, want %t:\n%s", test.Name, got.Pinned, test.WantPinned,
					buf.String())
			}
			if (got.Note != "") != test.WantNote {
				t.Errorf("%s: note = %q, want a note %t", test.Name, got.Note, test.WantNote)
			}
		})
	}
}
