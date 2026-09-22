package audit_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/identity"
	"github.com/kordloom/switchtender/internal/audit"
)

// TestAReceiptIssuedBeforeTheIdWidenedStillVerifies covers the promise attached to every receipt:
// that it verifies offline for as long as somebody keeps it.
//
// The install id binds a bundle's claims to the key that signed them, and it was widened from six
// bytes of the public key to a hash of the whole key, because six bytes is 48 bits and an attacker
// can grind a keypair born to a chosen id. Widening the derivation without accepting the old width
// broke every receipt already issued: the new binary derived the wide id, the old bundle named the
// narrow one, and verification reported a rotated install, which is the accusation of tampering the
// format exists to make meaningful. An operator upgrading would watch their own evidence start
// failing.
//
// The upgrade path is exercised against the previous release in public CI, which is where this was
// caught rather than here, so it is pinned here now.
func TestAReceiptIssuedBeforeTheIdWidenedStillVerifies(t *testing.T) {
	// No t.Parallel: this sets the audit key environment for the identity it loads.
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	at := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

	entry := &audit.Entry{ID: "e1", At: at, Actor: "casey", Method: "POST", Path: "/v1/runs"}
	audit.Link(nil, entry)

	legacy := identity.LegacyInstallIDFromKey(id.Public())
	if legacy == "" || len(legacy) != len("in_")+12 {
		t.Fatalf("the legacy derivation produced %q, want the six-byte form this test is about", legacy)
	}
	if legacy == identity.InstallIDFromKey(id.Public()) {
		t.Fatal("the two derivations agree, so this test cannot tell them apart")
	}

	doc, err := audit.BuildBundle([]*audit.Entry{entry}, id, "1.94.0", at)
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	// What a release before the widening wrote: the narrow id, from this same key, in both places
	// the bundle records it.
	doc.Producer.InstallID = legacy
	if doc.Chain != nil && doc.Chain.Params["install_id"] != "" {
		doc.Chain.Params["install_id"] = legacy
	}
	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}

	rep, err := audit.VerifyBundle(signed, id.KeyID())
	if err != nil {
		t.Fatalf("a receipt issued before the install id widened no longer verifies: %v.\n"+
			"Every receipt an install has already handed out stops verifying the moment its "+
			"binary is upgraded, which is the one thing a receipt promises not to do", err)
	}
	if !rep.OK() {
		t.Errorf("the receipt verifies but does not report OK: %+v", rep)
	}

	// A bundle naming an install this key was never born to, at either width, is still refused.
	doc2, err := audit.BuildBundle([]*audit.Entry{entry}, id, "1.94.0", at)
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	doc2.Producer.InstallID = "in_ffffffffffff"
	if doc2.Chain != nil && doc2.Chain.Params["install_id"] != "" {
		doc2.Chain.Params["install_id"] = "in_ffffffffffff"
	}
	lifted, err := audit.SignBundleDoc(doc2, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}
	if _, err := audit.VerifyBundle(lifted, id.KeyID()); err == nil ||
		!strings.Contains(err.Error(), "install") {
		t.Errorf("a bundle naming somebody else's install verified: %v", err)
	}
}
