package audit_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// TestPinnedKeyRefusesAForgedProducer is the attack the pin exists to stop.
//
// An attacker signs a bundle with a key of their own, then writes the victim's published fingerprint
// into producer.key_id. The signature is real, so it verifies against the public key the attacker
// also embedded. The pin compared the fingerprint a relying party supplied against that DECLARED
// string and never asked whether the embedded key was the key that string names, so the two halves
// of the check never met. The bundle reported as verified and pinned.
//
// This matters more than an ordinary verification bug: --pubkey is the only provenance tie the
// command's own help offers, so the one mechanism a relying party is told to use proved nothing.
// A bundle is worth exactly what its weakest accepted signature is worth.
func TestPinnedKeyRefusesAForgedProducer(t *testing.T) {
	victim := newTestIdentity(t, "victim")
	attacker := newTestIdentity(t, "attacker")

	chain := bundleChain(t)
	doc, err := audit.BuildBundle(chain, attacker, "1.34.1",
		time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	// Everything the attacker controls: their own key stays embedded, so the signature is genuine,
	// and only the advertised fingerprint is swapped for the victim's.
	doc.Producer.KeyID = victim.KeyID()
	signed, err := audit.SignBundleDoc(doc, attacker.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}

	rep, err := audit.VerifyBundle(signed, victim.KeyID())
	if err == nil {
		t.Fatalf("a bundle signed by another key verified against the victim's pin: %+v", rep)
	}
	if !strings.Contains(err.Error(), "key") {
		t.Errorf("refusal = %v, want it to name the key mismatch", err)
	}
}

// TestUnpinnedVerifyStillRefusesAMismatchedKeyID checks the same inconsistency is refused with no
// pin supplied at all, since a bundle whose advertised fingerprint is not its embedded key is
// malformed however it is read.
func TestUnpinnedVerifyStillRefusesAMismatchedKeyID(t *testing.T) {
	attacker := newTestIdentity(t, "attacker")
	chain := bundleChain(t)
	doc, err := audit.BuildBundle(chain, attacker, "1.34.1",
		time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	doc.Producer.KeyID = "sha256:" + strings.Repeat("0", 64)
	signed, err := audit.SignBundleDoc(doc, attacker.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}

	if _, err := audit.VerifyBundle(signed, ""); err == nil {
		t.Error("a bundle whose key_id does not name its embedded key was accepted")
	}
}

// TestHonestBundleStillVerifiesUnderItsOwnPin is the anti-overfit control. A refusal that refused
// everything would satisfy the tests above and break the product.
func TestHonestBundleStillVerifiesUnderItsOwnPin(t *testing.T) {
	id := newTestIdentity(t, "honest")
	chain := bundleChain(t)
	doc, err := audit.BuildBundle(chain, id, "1.34.1",
		time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}

	rep, err := audit.VerifyBundle(signed, id.KeyID())
	if err != nil {
		t.Fatalf("an honest bundle was refused under its own pin: %v", err)
	}
	if !rep.SignatureOK {
		t.Error("an honest bundle reported a bad signature")
	}
}

// newTestIdentity mints an identity in its own directory, so two of them in one test are genuinely
// different keys rather than the same one loaded twice.
func newTestIdentity(t *testing.T, name string) audit.Identity {
	t.Helper()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity(%s) error = %v", name, err)
	}
	return id
}
