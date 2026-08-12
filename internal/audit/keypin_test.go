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

// TestForgedChainHeadIsRefused pins that the head a bundle advertises has to be the head its claims
// actually reach.
//
// Nothing reconciled the two. A producer, or anyone who could re-sign, could publish a head naming a
// sequence and link the claims never produce, and the bundle verified. The anchor check made it
// worse by seeding its lookup from that same declared head, so an anchor over the forged head
// validated against the forgery rather than against the record. The head is the value a reader
// quotes as "where the log stood", so a head nobody can check is the one number in the document that
// has to be checkable.
func TestForgedChainHeadIsRefused(t *testing.T) {
	id := newTestIdentity(t, "producer")
	chain := bundleChain(t)
	doc, err := audit.BuildBundle(chain, id, "1.34.1",
		time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	doc.Chain.Head.Link = strings.Repeat("0", 64)
	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}

	rep, err := audit.VerifyBundle(signed, "")
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if rep.ChainOK {
		t.Error("a bundle whose head is not the head its claims reach reported a good chain")
	}
}

// TestWindowBundleWithALeadingHeadStillVerifies is the anti-overfit control for the head check.
//
// A bundle is often a window into a longer chain, so its head legitimately names a point later than
// anything the document discloses. Refusing that would break every window export while looking like
// a security improvement, which is the more expensive mistake of the two.
func TestWindowBundleWithALeadingHeadStillVerifies(t *testing.T) {
	id := newTestIdentity(t, "producer")
	chain := bundleChain(t)
	doc, err := audit.BuildBundle(chain, id, "1.34.1",
		time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	// The chain has moved on since this window was cut.
	doc.Chain.Head.Seq = doc.Chain.Head.Seq + 5
	doc.Chain.Head.Link = strings.Repeat("a", 64)
	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}

	rep, err := audit.VerifyBundle(signed, "")
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if !rep.ChainOK {
		t.Errorf("a window bundle whose head leads its claims was refused at seq %d", rep.BrokeAtSeq)
	}
}

// TestHeadBehindTheNewestClaimIsRefused pins the other direction. A chain only grows, so a head
// earlier than a claim the bundle carries describes a log that went backwards.
func TestHeadBehindTheNewestClaimIsRefused(t *testing.T) {
	id := newTestIdentity(t, "producer")
	chain := bundleChain(t)
	doc, err := audit.BuildBundle(chain, id, "1.34.1",
		time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	doc.Chain.Head.Seq = doc.Chain.Head.Seq - 1
	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}

	rep, err := audit.VerifyBundle(signed, "")
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if rep.ChainOK {
		t.Error("a head behind the newest claim reported a good chain")
	}
}

// TestAnchorOverAnUnprovenHeadIsRefused covers the anchor half of the same problem.
//
// A window bundle's head legitimately names a point later than anything the document discloses, so
// the head itself cannot be checked from the bundle. The anchor lookup was seeded from that declared
// head anyway, so an anchor over it validated against a value only the producer asserts. An anchor
// is meant to be independent evidence about the record; checking one against the producer's own
// unproven claim makes it a second copy of that claim instead.
func TestAnchorOverAnUnprovenHeadIsRefused(t *testing.T) {
	id := newTestIdentity(t, "producer")
	chain := bundleChain(t)
	doc, err := audit.BuildBundle(chain, id, "1.34.1",
		time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	// A head the claims cannot corroborate, with an anchor pointing at exactly that head.
	ahead := doc.Chain.Head.Seq + 5
	unproven := strings.Repeat("b", 64)
	doc.Chain.Head.Seq, doc.Chain.Head.Link = ahead, unproven
	doc.Anchors = append(doc.Anchors, audit.BundleAnchor{
		Type: "rfc3161", Seq: ahead, Link: unproven, At: "2026-08-11T16:00:00Z", Ref: "tsa",
	})
	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}

	rep, err := audit.VerifyBundle(signed, "")
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if rep.AnchorsOK {
		t.Error("an anchor over a head the bundle cannot prove was reported as satisfied")
	}
}

// TestSparseReceiptVerifies pins that a sparse receipt passes the verifier the tool tells a reader
// to use.
//
// The verifier had no profile dispatch: it recomputed the linear link over every claim, which a tree
// claim never satisfies because a tree claim carries a leaf hash and no previous link. So the
// command refused the output of "switchtender receipt --sparse", which prints "Verify it with:
// switchtender verify". A verifier that rejects its own product teaches a reader that a red result
// means nothing, which is worse than having no verifier.
func TestSparseReceiptVerifies(t *testing.T) {
	id := newTestIdentity(t, "producer")
	chain := bundleChain(t)

	doc, err := audit.BuildTreeBundle(chain, map[int64]bool{2: true}, id, "1.34.1",
		audit.BundleSubject{Type: "run", ID: "run_1"},
		time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildTreeBundle() error = %v", err)
	}
	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}

	rep, err := audit.VerifyBundle(signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle() refused a sparse receipt: %v", err)
	}
	if !rep.ChainOK {
		t.Errorf("a sparse receipt reported a broken chain at seq %d", rep.BrokeAtSeq)
	}
	if !rep.SignatureOK {
		t.Error("a sparse receipt reported a bad signature")
	}
}

// TestLiftedSparseReceiptIsRefused pins that a receipt is tied to the install that signed it.
//
// The leaves are hashed under an install id, and the bundle also carries one in its chain params. If
// the verifier folds using the params rather than the producer, someone can lift another install's
// receipt, rewrite only the producer block, sign it with their own key, and every leaf still folds,
// because the leaves keep hashing under the original install. The receipt then reads as evidence
// about a run on a machine that never ran it. Requiring the two to agree is what closes it.
func TestLiftedSparseReceiptIsRefused(t *testing.T) {
	victim := newTestIdentity(t, "victim")
	attacker := newTestIdentity(t, "attacker")
	chain := bundleChain(t)

	doc, err := audit.BuildTreeBundle(chain, map[int64]bool{2: true}, victim, "1.34.1",
		audit.BundleSubject{Type: "run", ID: "run_1"},
		time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildTreeBundle() error = %v", err)
	}
	// The attacker publishes the victim's receipt as their own, honestly signed by their own key.
	doc.Producer.InstallID = attacker.InstallID
	doc.Producer.PublicKey = attacker.PublicKeyBase64()
	doc.Producer.KeyID = attacker.KeyID()
	signed, err := audit.SignBundleDoc(doc, attacker.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}

	rep, err := audit.VerifyBundle(signed, attacker.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if rep.ChainOK {
		t.Error("a receipt lifted from another install verified under the copier's own key")
	}
}

// TestSparseReceiptWithDisagreeingInstallIsRefused gates the rule that the install a receipt is
// bound to and the install its chain params declare must be the same one.
//
// The fold already refuses a receipt lifted wholesale, because the leaves are hashed under the
// producer's install and stop folding the moment the producer changes. This is the other half: a
// bundle whose params name a different install than its producer is malformed however it folds, and
// the reference verifier says so, so a document that disagrees with itself must not read as
// verified here either.
func TestSparseReceiptWithDisagreeingInstallIsRefused(t *testing.T) {
	id := newTestIdentity(t, "producer")
	chain := bundleChain(t)
	doc, err := audit.BuildTreeBundle(chain, map[int64]bool{2: true}, id, "1.34.1",
		audit.BundleSubject{Type: "run", ID: "run_1"},
		time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildTreeBundle() error = %v", err)
	}
	// Everything folds; only the declared install disagrees with the producer.
	doc.Chain.Params["install_id"] = "in_someoneelse"
	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}

	rep, err := audit.VerifyBundle(signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if rep.ChainOK {
		t.Error("a receipt whose params name a different install than its producer verified")
	}
}
