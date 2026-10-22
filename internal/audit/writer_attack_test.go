package audit_test

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kordloom/loomseal/seal"

	"github.com/kordloom/switchtender/internal/audit"
)

// startClaims returns the indexes of the receipt's START claims in chain order.
func startClaims(doc *audit.Bundle) []int {
	var at []int
	for i, c := range doc.Claims {
		if c.Payload["method"] == audit.MethodStart {
			at = append(at, i)
		}
	}
	return at
}

// firstStart returns the index of the receipt's first START claim.
func firstStart(t *testing.T, doc *audit.Bundle) int {
	t.Helper()
	at := startClaims(doc)
	if len(at) == 0 {
		t.Fatal("the receipt carries no START claim")
	}
	return at[0]
}

// dropClaim removes claim i from the receipt, as a writer who deletes its entry from the chain
// leaves the segment the receipt is drawn from.
func dropClaim(doc *audit.Bundle, i int) {
	doc.Claims = append(doc.Claims[:i], doc.Claims[i+1:]...)
}

// TestAnEditedReceiptIsRefused is unit 10's table for the receipt: each row is an edit a principal
// who can write the chain's table and not the producer key makes to the entries a receipt is drawn
// from, carried into a receipt the producer signs as it would any other. Each is refused by the
// verifier, with the reason named. The rows the release rule owns are in
// TestReleaseClaimsAreHeldToTheOutcome; these are the START entries one attempt of a step commits,
// and the chain segment around them. A writer who also rewrites every link after an edit is caught
// by an anchor over the head, not by the segment, so no row here relinks, and no row cuts the
// segment's tail, which leaves a receipt of a run that has not ended.
//
//nolint:funlen // Test function.
func TestAnEditedReceiptIsRefused(t *testing.T) {
	t.Parallel()
	signed, err := os.ReadFile(filepath.Join("testdata", releaseApproved))
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	other := ed25519.NewKeyFromSeed([]byte(strings.Repeat("w", ed25519.SeedSize)))
	tests := []struct {
		// Name labels the row.
		Name string
		// Edit changes the receipt before it is signed again, nil to verify it as produced.
		Edit func(t *testing.T, doc *audit.Bundle)
		// SignWith is the key the edited receipt is signed with, nil for the producer's.
		SignWith ed25519.PrivateKey
		// WantOK reports whether the receipt verifies.
		WantOK bool
		// WantProblem is a phrase the chain problem carries, empty for none.
		WantProblem string
	}{{ // Test 0: The receipt as produced verifies, carrying a START for each step that ran.
		Name:   "as produced",
		WantOK: true,
	}, { // Test 1: A START entry moved to another attempt of its step.
		Name: "start attempt edited",
		Edit: func(t *testing.T, doc *audit.Bundle) {
			c := doc.Claims[firstStart(t, doc)].Payload
			path, _ := c["path"].(string)
			c["path"] = strings.TrimSuffix(path, "/0") + "/1"
		},
		WantProblem: "does not recompute to its link",
	}, { // Test 2: A START entry moved to another run.
		Name: "start run edited",
		Edit: func(t *testing.T, doc *audit.Bundle) {
			doc.Claims[firstStart(t, doc)].Payload["path"] = audit.StartPath("run_elsewhere", 0)
		},
		WantProblem: "does not recompute to its link",
	}, { // Test 3: A START entry's committed body replaced.
		Name: "start digest edited",
		Edit: func(t *testing.T, doc *audit.Bundle) {
			doc.Claims[firstStart(t, doc)].Payload["content_digest"] = "sha256:" +
				strings.Repeat("ab", 32)
		},
		WantProblem: "does not recompute to its link",
	}, { // Test 4: A START entry deleted, which would let the attempt start again.
		Name: "start dropped",
		Edit: func(t *testing.T, doc *audit.Bundle) {
			dropClaim(doc, firstStart(t, doc))
		},
		WantProblem: "is missing",
	}, { // Test 5: The approval step's decision deleted from between its request and the release.
		Name: "step decision dropped",
		Edit: func(t *testing.T, doc *audit.Bundle) {
			for i, c := range doc.Claims {
				path, _ := c.Payload["path"].(string)
				if c.Payload["method"] == audit.MethodDecision && strings.Contains(path, "/steps/") &&
					strings.HasSuffix(path, "/decision/approved") {
					dropClaim(doc, i)
					return
				}
			}
			t.Fatal("the receipt carries no step approval")
		},
		WantProblem: "is missing",
	}, { // Test 6: A START entry moved ahead of the release it follows.
		Name: "start before its release",
		Edit: func(t *testing.T, doc *audit.Bundle) {
			i := firstStart(t, doc)
			doc.Claims[i-1], doc.Claims[i] = doc.Claims[i], doc.Claims[i-1]
		},
		WantProblem: "so the entries are out of order",
	}, { // Test 7: The receipt signed again by a key that is not the producer's.
		Name:     "signed by another key",
		Edit:     func(*testing.T, *audit.Bundle) {},
		SignWith: other,
	}}
	pub, _ := releaseKey().Public().(ed25519.PublicKey)
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := signed
			if test.Edit != nil {
				var bundle audit.Bundle
				if err := json.Unmarshal(signed, &bundle); err != nil {
					t.Fatalf("decode the fixture: %v", err)
				}
				test.Edit(t, &bundle)
				key := test.SignWith
				if key == nil {
					key = releaseKey()
				}
				var err error
				if doc, err = audit.SignBundleDoc(&bundle, key); err != nil {
					t.Fatalf("SignBundleDoc() error = %v", err)
				}
			}
			rep, err := audit.VerifyBundle(doc, seal.KeyID(pub))
			if err != nil {
				t.Fatalf("VerifyBundle() error = %v", err)
			}
			if wantSigned := test.SignWith == nil; rep.SignatureOK != wantSigned {
				t.Errorf("SignatureOK = %t, want %t", rep.SignatureOK, wantSigned)
			}
			if got := rep.OK(); got != test.WantOK {
				t.Errorf("OK() = %t, want %t (signature %t, chain %t: %s)", got, test.WantOK,
					rep.SignatureOK, rep.ChainOK, rep.ChainProblem)
			}
			if !strings.Contains(rep.ChainProblem, test.WantProblem) {
				t.Errorf("chain problem = %q, want one saying %q", rep.ChainProblem, test.WantProblem)
			}
			if test.WantOK {
				var bundle audit.Bundle
				if err := json.Unmarshal(doc, &bundle); err != nil {
					t.Fatalf("decode the receipt: %v", err)
				}
				if n := len(startClaims(&bundle)); n != 2 {
					t.Errorf("the receipt carries %d START claims, want one for build and deploy", n)
				}
			}
		})
	}
}
