package audit_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// TestEveryBundleShapeAgreesWithTheReferenceVerifier runs every bundle this product emits through
// both verifiers and requires them to reach the same answer.
//
// The reference verifier was reachable from exactly one test, over one bundle shape. Everything else
// this product produces was only ever checked by the checker that shipped beside it, and it had
// drifted: it accepted a bundle signed by the wrong key, accepted a forged head, and rejected its
// own sparse receipt. Each of those is a case where these two verifiers disagreed and nothing asked
// them to agree.
//
// Two verifiers that must agree is the whole point. One of them is the specification's, so a
// disagreement is either a bug here or a bug in the format, and both are worth stopping at the
// suite rather than at somebody auditing a deployment.
func TestEveryBundleShapeAgreesWithTheReferenceVerifier(t *testing.T) {
	id := treeIdentity(t)
	chain := treeChain(t, 6)
	at := time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)

	shapes := []struct {
		Name  string
		Build func(t *testing.T) []byte
	}{
		{
			Name: "the whole linear audit chain",
			Build: func(t *testing.T) []byte {
				t.Helper()
				doc, err := audit.BuildBundle(chain, id, "1.34.1", at)
				if err != nil {
					t.Fatalf("BuildBundle() error = %v", err)
				}
				return signBundle(t, doc, id)
			},
		},
		{
			Name: "a window into a longer linear chain",
			Build: func(t *testing.T) []byte {
				t.Helper()
				doc, err := audit.BuildBundle(chain[2:], id, "1.34.1", at)
				if err != nil {
					t.Fatalf("BuildBundle(window) error = %v", err)
				}
				return signBundle(t, doc, id)
			},
		},
		{
			Name: "a sparse receipt over one entry",
			Build: func(t *testing.T) []byte {
				t.Helper()
				doc, err := audit.BuildTreeBundle(chain, map[int64]bool{3: true}, id, "1.34.1",
					audit.BundleSubject{Type: "run", ID: "run_3"}, at)
				if err != nil {
					t.Fatalf("BuildTreeBundle() error = %v", err)
				}
				return signBundle(t, doc, id)
			},
		},
		{
			Name: "a sparse receipt over several entries",
			Build: func(t *testing.T) []byte {
				t.Helper()
				doc, err := audit.BuildTreeBundle(chain, map[int64]bool{1: true, 4: true, 6: true},
					id, "1.34.1", audit.BundleSubject{Type: "fleet", ID: id.InstallID}, at)
				if err != nil {
					t.Fatalf("BuildTreeBundle(several) error = %v", err)
				}
				return signBundle(t, doc, id)
			},
		},
	}
	for _, shape := range shapes {
		t.Run(shape.Name, func(t *testing.T) {
			signed := shape.Build(t)

			theirs := verifyWithLoomSeal(t, signed)
			ours, err := audit.VerifyBundle(signed, id.KeyID())

			if !theirs.OK {
				t.Fatalf("the reference verifier refused a bundle this product emits: %v",
					theirs.Problems)
			}
			if err != nil {
				t.Fatalf("our verifier refused a bundle the reference verifier accepts: %v", err)
			}
			if !ours.SignatureOK || !ours.ChainOK {
				t.Errorf("our verifier reports signature=%v chain=%v on a bundle the reference "+
					"verifier accepts", ours.SignatureOK, ours.ChainOK)
			}
		})
	}
}

// TestBothVerifiersRejectTheSameTampering is the other half. Agreeing that good bundles are good is
// easy; a verifier that returned true unconditionally would pass the test above. These are the
// tamperings that were accepted here while the reference verifier refused them, so each row is a
// disagreement that actually happened rather than one imagined for the test.
func TestBothVerifiersRejectTheSameTampering(t *testing.T) {
	id := treeIdentity(t)
	other := treeIdentity(t)
	chain := treeChain(t, 6)
	at := time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)

	tamper := []struct {
		Name string
		Do   func(b *audit.Bundle)
	}{
		{
			Name: "the producer advertises a key it does not carry",
			Do:   func(b *audit.Bundle) { b.Producer.KeyID = other.KeyID() },
		},
		{
			Name: "the head names a link the claims never reach",
			Do:   func(b *audit.Bundle) { b.Chain.Head.Link = strings.Repeat("0", 64) },
		},
		{
			Name: "a claim's payload is edited after the fact",
			Do:   func(b *audit.Bundle) { b.Claims[1].Payload["path"] = "/v1/runs/somewhere-else" },
		},
	}
	for _, tc := range tamper {
		t.Run(tc.Name, func(t *testing.T) {
			doc, err := audit.BuildBundle(chain, id, "1.34.1", at)
			if err != nil {
				t.Fatalf("BuildBundle() error = %v", err)
			}
			tc.Do(doc)
			// Re-signed after tampering, so no row passes merely because the signature broke.
			signed := signBundle(t, doc, id)

			theirs := verifyWithLoomSeal(t, signed)
			if theirs.OK {
				t.Fatalf("the reference verifier accepted the tampering, so this test is not "+
					"describing the format's rule: %s", tc.Name)
			}
			rep, err := audit.VerifyBundle(signed, "")
			if err == nil && rep.SignatureOK && rep.ChainOK {
				t.Errorf("our verifier accepted what the reference verifier refused: %v",
					theirs.Problems)
			}
		})
	}
}

// signBundle signs a bundle document with the identity's key and fails the test if it cannot.
func signBundle(t *testing.T, doc *audit.Bundle, id audit.Identity) []byte {
	t.Helper()
	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}
	return signed
}
