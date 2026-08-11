package audit_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// signedBundle builds and signs a bundle over the shared test chain, returning the bytes and the
// identity that signed it so a test can re-sign a tampered variant.
func signedBundle(t *testing.T) ([]byte, audit.Identity, *audit.Bundle) {
	t.Helper()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	b, err := audit.BuildBundle(bundleChain(t), id, "v-test", time.Date(2026, 7, 27, 16, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	signed, err := audit.SignBundleDoc(b, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}
	return signed, id, b
}

// TestVerifyBundleAcceptsAGenuineBundle proves a bundle this install signed verifies offline with no
// store and no network: the signature covers it, every chain link recomputes, and the pinned key
// matches. This is the yes a receipt's verify command answers.
func TestVerifyBundleAcceptsAGenuineBundle(t *testing.T) {
	signed, id, _ := signedBundle(t)

	rep, err := audit.VerifyBundle(signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if !rep.OK() {
		t.Errorf("report = %+v, want every check to pass", rep)
	}
	if !rep.SignatureOK || !rep.ChainOK || !rep.AnchorsOK {
		t.Errorf("signature=%v chain=%v anchors=%v, want all true",
			rep.SignatureOK, rep.ChainOK, rep.AnchorsOK)
	}
	if rep.ClaimCount != 4 {
		t.Errorf("claim count = %d, want 4", rep.ClaimCount)
	}
}

// TestVerifyBundleCatchesASignatureThatDoesNotCover flips a byte of the signature and checks the
// bundle no longer verifies, so a bundle altered after signing is caught.
func TestVerifyBundleCatchesASignatureThatDoesNotCover(t *testing.T) {
	signed, _, _ := signedBundle(t)

	var doc map[string]any
	if err := json.Unmarshal(signed, &doc); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	sig := doc["signatures"].([]any)[0].(map[string]any)
	s := sig["sig"].(string)
	// Flip the first base64 character to a different one.
	flip := "B"
	if strings.HasPrefix(s, "B") {
		flip = "C"
	}
	sig["sig"] = flip + s[1:]
	tampered, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	rep, err := audit.VerifyBundle(tampered, "")
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if rep.SignatureOK {
		t.Error("a bundle with a broken signature reported SignatureOK")
	}
	if rep.OK() {
		t.Error("a bundle with a broken signature reported OK")
	}
}

// TestVerifyBundleCatchesATamperedClaim re-signs a bundle whose claim payload was altered, so the
// signature is valid but a link no longer recomputes. This isolates the chain check from the
// signature check: a producer who edits a past entry and re-signs is still caught.
func TestVerifyBundleCatchesATamperedClaim(t *testing.T) {
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	b, err := audit.BuildBundle(bundleChain(t), id, "v-test", time.Date(2026, 7, 27, 16, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	// Edit a claim's recorded path but leave its committed link, then re-sign so the signature is
	// genuine over the altered bundle.
	b.Claims[1].Payload["path"] = "/v1/evil"
	signed, err := audit.SignBundleDoc(b, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}

	rep, err := audit.VerifyBundle(signed, "")
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if !rep.SignatureOK {
		t.Error("the re-signed bundle should have a valid signature")
	}
	if rep.ChainOK {
		t.Error("a bundle whose claim was altered reported ChainOK")
	}
	if rep.BrokeAtSeq != b.Claims[1].Chain.Seq {
		t.Errorf("broke at seq %d, want %d", rep.BrokeAtSeq, b.Claims[1].Chain.Seq)
	}
}

// TestVerifyBundleRefusesAnUnpinnedKey checks a bundle signed by a different key than the one a
// relying party pinned is refused before its own signature is trusted.
func TestVerifyBundleRefusesAnUnpinnedKey(t *testing.T) {
	signed, _, _ := signedBundle(t)
	if _, err := audit.VerifyBundle(signed, "sha256:not-the-key"); !errors.Is(err, audit.ErrVerify) {
		t.Errorf("VerifyBundle() with a wrong pinned key error = %v, want ErrVerify", err)
	}
}
