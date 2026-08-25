package audit_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// TestALinearReceiptCannotBeLiftedByAnotherInstall covers the theft the tree profile was already
// protected from and the linear one was not.
//
// A linear chain link is a hash of the entry's own fields, so it commits to nothing about who
// produced it, and the linear profile stated no install. Install B could therefore download install
// A's published receipt, keep A's claims and A's genuine third-party timestamp anchor, rewrite only
// the producer block, and re-sign with B's key. A relying party pinning B's published fingerprint
// then read A's history as B's own, with a real anchor vouching for it. The tree profile binds its
// leaves to the producer's install id for exactly this reason; this is the same binding stated for
// the shape that `switchtender receipt` emits by default.
func TestALinearReceiptCannotBeLiftedByAnotherInstall(t *testing.T) {
	// No t.Parallel: treeIdentity uses t.Setenv.
	victim := treeIdentity(t)
	thief := treeIdentity(t)
	if victim.InstallID == thief.InstallID {
		t.Fatal("the two test identities share an install id, so this proves nothing")
	}
	chain := treeChainFor(t, 4, victim.InstallID)
	at := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

	// The genuine receipt verifies under the install that made it.
	doc, err := audit.BuildBundle(chain, victim, "1.67.0", at)
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	honest := signBundle(t, doc, victim)
	rep, err := audit.VerifyBundle(honest, victim.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle(honest) error = %v", err)
	}
	if !rep.OK() {
		t.Fatalf("the install's own receipt does not verify: %+v", rep)
	}

	// The theft: keep everything, swap the producer, re-sign as the thief.
	var raw map[string]any
	if err := json.Unmarshal(honest, &raw); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	producer, ok := raw["producer"].(map[string]any)
	if !ok {
		t.Fatalf("no producer block in %s", honest)
	}
	// A self-consistent forgery: the thief presents their own key and their own fingerprint, so the
	// key-id check that catches a mismatched pair has nothing to complain about. The only thing left
	// that can object is the chain naming the install it belongs to.
	producer["install_id"] = thief.InstallID
	producer["key_id"] = thief.KeyID()
	producer["public_key"] = thiefPublicKey(t, thief)
	delete(raw, "signatures")

	lifted, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var stolen audit.Bundle
	if err := json.Unmarshal(lifted, &stolen); err != nil {
		t.Fatalf("Unmarshal(stolen) error = %v", err)
	}
	resigned := signBundle(t, &stolen, thief)

	// A relying party pinning the thief's own fingerprint must not accept the victim's history.
	got, err := audit.VerifyBundle(resigned, thief.KeyID())
	if err == nil && got.OK() {
		t.Error("a receipt lifted from another install verified under the thief's own key, so its " +
			"history and its third-party anchor now vouch for work it never did")
	}
}

// thiefPublicKey returns an identity's public key in the base64 encoding the bundle format uses, so
// a forged producer block is internally consistent rather than caught by the key-id check.
func thiefPublicKey(t *testing.T, id audit.Identity) string {
	t.Helper()
	doc, err := audit.BuildBundle(treeChain(t, 1), id, "1.67.0", time.Now())
	if err != nil {
		t.Fatalf("BuildBundle() for key extraction error = %v", err)
	}
	return doc.Producer.PublicKey
}

// TestLiftingByRewritingBothInstallIDsIsRefused is the same theft by a thief who reads the check.
//
// Comparing a stated install against the producer catches a forger who rewrote one of them. What
// stops the one who rewrites both is that the id is folded into the link preimage: change it and
// every link stops recomputing, and recomputing them breaks any genuine third-party anchor over the
// old head. The check and the binding are two halves of one thing, and this covers the half the
// equality test cannot reach.
//
// An entry written before the binding existed carries no install and stays liftable. That cannot be
// fixed after the fact, since a link already written commits to what it committed to, and it is
// covered separately below.
func TestLiftingByRewritingBothInstallIDsIsRefused(t *testing.T) {
	victim := treeIdentity(t)
	thief := treeIdentity(t)
	chain := treeChainFor(t, 4, victim.InstallID)
	at := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

	doc, err := audit.BuildBundle(chain, victim, "1.67.0", at)
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	honest := signBundle(t, doc, victim)

	var raw map[string]any
	if err := json.Unmarshal(honest, &raw); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	producer := raw["producer"].(map[string]any)
	producer["install_id"] = thief.InstallID
	producer["key_id"] = thief.KeyID()
	producer["public_key"] = thiefPublicKey(t, thief)
	// The difference from the test above: the thief rewrites the chain's stated install too.
	if chainBlock, ok := raw["chain"].(map[string]any); ok {
		if params, ok := chainBlock["params"].(map[string]any); ok {
			params["install_id"] = thief.InstallID
		}
	}
	delete(raw, "signatures")

	lifted, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var stolen audit.Bundle
	if err := json.Unmarshal(lifted, &stolen); err != nil {
		t.Fatalf("Unmarshal(stolen) error = %v", err)
	}
	resigned := signBundle(t, &stolen, thief)

	got, err := audit.VerifyBundle(resigned, thief.KeyID())
	if err == nil && got.OK() {
		t.Error("a receipt lifted with both install ids rewritten verified under the thief's own " +
			"key, so the link does not commit to who produced it")
	}
}

// TestAPreBindingReceiptRemainsLiftable states plainly what the binding does not reach.
//
// An entry written before the install was folded into the link carries none, hashes as it always
// did, and is bound to nobody. A receipt made only of those can still be lifted. Recording it is
// the honest position: the alternative is re-anchoring every existing chain, and a link already
// written commits to what it committed to whatever is decided later.
func TestAPreBindingReceiptRemainsLiftable(t *testing.T) {
	victim := treeIdentity(t)
	thief := treeIdentity(t)
	// No install on the entries: the shape every chain had before the binding.
	chain := treeChainFor(t, 4, "")
	at := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

	doc, err := audit.BuildBundle(chain, victim, "1.68.0", at)
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	honest := signBundle(t, doc, victim)

	var raw map[string]any
	if err := json.Unmarshal(honest, &raw); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	producer := raw["producer"].(map[string]any)
	producer["install_id"] = thief.InstallID
	producer["key_id"] = thief.KeyID()
	producer["public_key"] = thiefPublicKey(t, thief)
	if chainBlock, ok := raw["chain"].(map[string]any); ok {
		if params, ok := chainBlock["params"].(map[string]any); ok {
			params["install_id"] = thief.InstallID
		}
	}
	delete(raw, "signatures")
	lifted, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var stolen audit.Bundle
	if err := json.Unmarshal(lifted, &stolen); err != nil {
		t.Fatalf("Unmarshal(stolen) error = %v", err)
	}

	got, err := audit.VerifyBundle(signBundle(t, &stolen, thief), thief.KeyID())
	if err != nil || !got.OK() {
		t.Errorf("a pre-binding receipt no longer verifies after being lifted (err=%v). If old "+
			"entries are now bound somehow, this test should become a refusal", err)
	}
}
