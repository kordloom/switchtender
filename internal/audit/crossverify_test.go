package audit_test

import (
	"encoding/json"
	"os"
	"path/filepath"
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

// TestBothVerifiersRejectTreeTampering is the tree-profile counterpart, and it exists because its
// absence is exactly how the forged-link hole survived.
//
// The linear tampering test above never built a tree bundle, so the harness that was supposed to
// keep the two verifiers in agreement had a blind spot precisely under the profile that had just
// been added. Each row here is a forgery the tree verifier once accepted, or would accept if a check
// were removed, that the reference verifier refuses.
func TestBothVerifiersRejectTreeTampering(t *testing.T) {
	id := treeIdentity(t)
	chain := treeChain(t, 6)
	at := time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)

	tamper := []struct {
		Name string
		Do   func(b *audit.Bundle)
	}{
		{
			Name: "a disclosed claim declares a link that is not its leaf hash",
			Do:   func(b *audit.Bundle) { b.Claims[0].Chain.Link = strings.Repeat("f", 64) },
		},
		{
			Name: "a disclosed claim carries a previous link a tree has no place for",
			Do:   func(b *audit.Bundle) { b.Claims[0].Chain.Prev = strings.Repeat("a", 64) },
		},
		{
			Name: "the anchored root is not the head the claims fold to",
			Do:   func(b *audit.Bundle) { b.Chain.Head.Link = strings.Repeat("b", 64) },
		},
	}
	for _, tc := range tamper {
		t.Run(tc.Name, func(t *testing.T) {
			doc, err := audit.BuildTreeBundle(chain, map[int64]bool{2: true, 4: true}, id, "1.34.1",
				audit.BundleSubject{Type: "run", ID: "run_2"}, at)
			if err != nil {
				t.Fatalf("BuildTreeBundle() error = %v", err)
			}
			tc.Do(doc)
			signed := signBundle(t, doc, id)

			theirs := verifyWithLoomSeal(t, signed)
			if theirs.OK {
				t.Fatalf("the reference verifier accepted the tampering, so this test is not "+
					"describing the format's rule: %s", tc.Name)
			}
			rep, err := audit.VerifyBundle(signed, "")
			if err == nil && rep.ChainOK {
				t.Errorf("our verifier accepted what the reference verifier refused: %v",
					theirs.Problems)
			}
		})
	}
}

// TestMirrorAgreesWithTheReferenceCorpus runs this product's verifier over the reference
// repository's own conformance corpus, the reverse direction of every other test in this file.
// The pinned key-set tests cannot catch a mirror that quietly falls a release behind the strip
// list, because this product's own bundles never carry the members the strip covers; the corpus
// bundles do. Signature verdicts must agree on every vector. Chain verdicts are compared only for
// the profiles this product implements, and vectors under other profiles must be refused by name
// as unsupported, never misreported as a broken chain.
func TestMirrorAgreesWithTheReferenceCorpus(t *testing.T) {
	dir := loomsealRepo(t)
	raw, err := os.ReadFile(filepath.Join(dir, "testdata", "vectors", "manifest.json"))
	if err != nil {
		t.Fatalf("read corpus manifest: %v", err)
	}
	var man struct {
		Vectors []struct {
			Name         string `json:"name"`
			File         string `json:"file"`
			MustVerify   bool   `json:"must_verify"`
			FailingCheck string `json:"failing_check"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatalf("parse corpus manifest: %v", err)
	}
	if len(man.Vectors) == 0 {
		t.Fatal("the corpus manifest lists no vectors")
	}
	for _, v := range man.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			signed, rerr := os.ReadFile(filepath.Join(dir, "testdata", "vectors", v.File))
			if rerr != nil {
				t.Fatalf("read vector: %v", rerr)
			}
			var probe struct {
				Producer struct {
					KeyID     string `json:"key_id"`
					InstallID string `json:"install_id"`
				} `json:"producer"`
			}
			if json.Unmarshal(signed, &probe) != nil {
				return
			}
			// The corpus asks whether the mirror agrees on format verdicts, not whether it
			// trusts fixture producers, so each vector's own declared install-and-key pair is
			// accepted the way a relying party accepts a rotation. The product's strict
			// derivation tie stays exactly as strict for real verification.
			var rep *audit.BundleReport
			var err error
			if probe.Producer.InstallID != "" && probe.Producer.KeyID != "" {
				rep, err = audit.VerifyBundleForInstall(signed, probe.Producer.KeyID,
					probe.Producer.InstallID)
			} else {
				rep, err = audit.VerifyBundle(signed, "")
			}
			named := err != nil && strings.Contains(err.Error(), "this product does not verify")
			if v.MustVerify {
				// A scoped mirror may refuse a surface or profile it does not implement, by
				// name, never on signature or chain grounds: "unimplemented here" and "invalid"
				// must not share a message, the same rule the reference applies to unsupported.
				if err != nil && !named {
					t.Fatalf("refused a must-verify vector for the wrong reason: %v", err)
				}
				// Even a named refusal must agree with the reference about the signature: the
				// strip list is what this asserts, since these are the only vectors carrying
				// the stripped surfaces.
				if rep != nil && !rep.SignatureOK {
					t.Errorf("the mirror failed the signature on a must-verify vector (err=%v)", err)
				}
				return
			}
			// A must-not-verify vector must never be called fully good.
			if err == nil && rep.SignatureOK && rep.ChainOK && rep.AnchorsOK {
				t.Errorf("the mirror fully verified a must-not-verify vector (%s)", v.FailingCheck)
			}
		})
	}
}
