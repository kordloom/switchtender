package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// writeFleetBundle signs a whole-install bundle over a chain of n entries after cut has had its way
// with the claims, writes it out, and returns its path and the key that signed it. The bundle is
// re-signed after the cut, the way a producer who edits its own export would do it, so the signature
// holds and only the chain can refuse it.
func writeFleetBundle(t *testing.T, n int, cut func([]audit.BundleClaim) []audit.BundleClaim) (path, keyID string) {
	t.Helper()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	at := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	entries := make([]*audit.Entry, 0, n)
	var prev *audit.Entry
	for i := range n {
		e := &audit.Entry{
			ID: audit.NewID(), At: at.Add(time.Duration(i) * time.Minute), Actor: "ops-token",
			ActorType: "token", Method: "POST", Path: fmt.Sprintf("/v1/templates/tpl_%d/launch", i),
			InstallID: id.InstallID,
		}
		audit.Link(prev, e)
		entries = append(entries, e)
		prev = e
	}
	doc, err := audit.BuildBundle(entries, id, "1.101.0", at.Add(time.Hour))
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	if doc.Subject.Type != "fleet" {
		t.Fatalf("the whole-install bundle names subject %q, want fleet", doc.Subject.Type)
	}
	if cut != nil {
		doc.Claims = cut(doc.Claims)
	}
	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}
	path = filepath.Join(t.TempDir(), "audit.loomseal.json")
	if err := os.WriteFile(path, signed, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path, id.KeyID()
}

// withoutSeqs returns a cut that drops the claims at the named sequence numbers.
func withoutSeqs(seqs ...int64) func([]audit.BundleClaim) []audit.BundleClaim {
	return func(claims []audit.BundleClaim) []audit.BundleClaim {
		return slices.DeleteFunc(claims, func(c audit.BundleClaim) bool {
			return slices.Contains(seqs, c.Chain.Seq)
		})
	}
}

// swapped returns a cut that exchanges the claims at two positions.
func swapped(i, j int) func([]audit.BundleClaim) []audit.BundleClaim {
	return func(claims []audit.BundleClaim) []audit.BundleClaim {
		claims[i], claims[j] = claims[j], claims[i]
		return claims
	}
}

// TestVerifySaysWhatIsMissingAndWhatItVerified pins two things the verify output got wrong about a
// whole-install bundle with an entry cut out of the middle and re-signed.
//
// It said "chain FAILED (does not recompute at seq 11)". Seq 11 recomputes. What is wrong is the seq
// 10 that is gone, and a reader sent to look at seq 11 finds nothing the matter with it. And it called
// the file a receipt from the pin line to the error, though a receipt is about one run and this
// bundle is about every change the install recorded. The last two cases hold the other half: a run's
// receipt keeps its name, so a fix that swapped one word for the other everywhere fails here too.
//
//nolint:funlen // Test function.
func TestVerifySaysWhatIsMissingAndWhatItVerified(t *testing.T) {
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Receipt verifies a run's receipt instead of a whole-install bundle.
		Receipt bool
		// Cut is what was done to the bundle's claims before it was re-signed, nil for nothing.
		Cut func([]audit.BundleClaim) []audit.BundleClaim
		// Pinned pins the key that signed the file.
		Pinned bool
		// WantLines must all appear in the output.
		WantLines []string
		// DenyWords must not appear in the output or the error.
		DenyWords []string
		// WantErr must appear in the returned error, empty when verification must pass.
		WantErr string
	}{{ // Test 0: One entry cut out names that entry, not the one after it.
		Name: "entry 10 dropped", Cut: withoutSeqs(10),
		WantLines: []string{"chain        FAILED (the entry at seq 10 is missing, so seq 11 does " +
			"not follow seq 9)", "NOT VERIFIED: the entry at seq 10 is missing"},
		DenyWords: []string{"does not recompute", "receipt"},
		WantErr:   "bundle did not verify: the entry at seq 10 is missing, so seq 11 does not follow seq 9",
	}, { // Test 1: A run of entries cut out names the whole run.
		Name: "entries 10 through 12 dropped", Cut: withoutSeqs(10, 11, 12),
		WantLines: []string{"chain        FAILED (the entries at seq 10 through 12 are missing, so " +
			"seq 13 does not follow seq 9)"},
		DenyWords: []string{"does not recompute", "receipt"},
		WantErr:   "bundle did not verify: the entries at seq 10 through 12 are missing",
	}, { // Test 2: An entry that is present but out of place is not called missing.
		Name: "entries 10 and 11 swapped", Cut: swapped(9, 10),
		WantLines: []string{"chain        FAILED (the entry at seq 11 comes before seq 10, so the " +
			"entries are out of order)"},
		DenyWords: []string{"does not recompute", "missing", "receipt"},
		WantErr:   "bundle did not verify: the entry at seq 11 comes before seq 10",
	}, { // Test 3: An intact bundle checked without a pin is an intact bundle, not a receipt.
		Name: "intact bundle unpinned",
		WantLines: []string{"pin          NONE (this says the bundle was signed, not who signed it)",
			"INTACT, BUT UNIDENTIFIED: nothing has been altered since this bundle was signed."},
		DenyWords: []string{"receipt"},
	}, { // Test 4: The same bundle pinned is a verified bundle.
		Name: "intact bundle pinned", Pinned: true,
		WantLines: []string{"VERIFIED: nothing has been altered since this bundle was signed"},
		DenyWords: []string{"receipt"},
	}, { // Test 5: A run's receipt checked without a pin is still called a receipt.
		Name: "receipt unpinned", Receipt: true,
		WantLines: []string{"pin          NONE (this says the receipt was signed, not who signed it)",
			"INTACT, BUT UNIDENTIFIED: nothing has been altered since this receipt was signed."},
		DenyWords: []string{"bundle"},
	}, { // Test 6: And pinned, it is a verified receipt.
		Name: "receipt pinned", Receipt: true, Pinned: true,
		WantLines: []string{"VERIFIED: nothing has been altered since this receipt was signed"},
		DenyWords: []string{"bundle"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			var path, keyID string
			if test.Receipt {
				path, keyID = writeGenuineReceipt(t)
			} else {
				path, keyID = writeFleetBundle(t, 14, test.Cut)
			}
			verifyPubkey = ""
			if test.Pinned {
				verifyPubkey = keyID
			}
			defer func() { verifyPubkey = "" }()
			var buf bytes.Buffer
			c := testCommand()
			c.SetOut(&buf)
			err := runVerify(c, []string{path})
			got := buf.String()
			switch {
			case test.WantErr == "" && err != nil:
				t.Fatalf("%s: runVerify() error = %v, want it to verify\n%s", test.Name, err, got)
			case test.WantErr != "" && err == nil:
				t.Fatalf("%s: runVerify() verified a file it had to refuse\n%s", test.Name, got)
			case test.WantErr != "" && !strings.Contains(err.Error(), test.WantErr):
				t.Errorf("%s: runVerify() error = %q, want it to say %q", test.Name, err, test.WantErr)
			}
			for _, want := range test.WantLines {
				if !strings.Contains(got, want) {
					t.Errorf("%s: output is missing %q:\n%s", test.Name, want, got)
				}
			}
			for _, deny := range test.DenyWords {
				if strings.Contains(got, deny) || (err != nil && strings.Contains(err.Error(), deny)) {
					t.Errorf("%s: output or error says %q (error %v):\n%s", test.Name, deny, err, got)
				}
			}
		})
	}
}
