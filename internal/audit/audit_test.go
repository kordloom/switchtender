package audit_test

import (
	"testing"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/audittest"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	audittest.Contract(t, func() audit.Store { return audit.NewMemStore() })
}

// TestContentDigestRedactsSecrets is the security property of the digest: a body's secret values do
// not enter it, so the digest stored in the chain and served in exports is not a brute-force target
// for a low-entropy secret. Two create-credential bodies that differ only in the secret must digest
// identically, and neither digest input may contain the secret.
func TestContentDigestRedactsSecrets(t *testing.T) {
	t.Parallel()
	// The digest is a keyed commitment now, so the property is proven by disclosure: a body that
	// differs only in a secret value verifies against the same commitment, because the secret was
	// replaced before the commitment was taken. If the secret influenced the commitment, disclosing
	// the other secret would fail.
	a := []byte(`{"name":"prod","kind":"ssh_key","secret":"hunter2","passphrase":"unlock-me"}`)
	b := []byte(`{"name":"prod","kind":"ssh_key","secret":"totally-different","passphrase":"other"}`)
	digest, nonce, err := audit.ContentDigestOf(a)
	if err != nil {
		t.Fatalf("ContentDigestOf() error = %v", err)
	}
	if digest == "" || !audit.VerifyContentDigest(digest, nonce, b) {
		t.Error("a body differing only in the secret does not verify; the secret leaked into the digest")
	}
	// A change in a non-secret field must NOT verify, or the commitment proves nothing about the
	// shape of the change.
	c := []byte(`{"name":"staging","kind":"ssh_key","secret":"hunter2","passphrase":"unlock-me"}`)
	if audit.VerifyContentDigest(digest, nonce, c) {
		t.Error("a body differing in the name still verified, so the commitment covers nothing useful")
	}
	// A nested secret, as a typed credential's fields object carries, is also redacted.
	nested := []byte(`{"name":"dd","type_id":"ct_1","fields":{"api_key":"sk-live-123"}}`)
	nested2 := []byte(`{"name":"dd","type_id":"ct_1","fields":{"api_key":"sk-live-999"}}`)
	nd, nn, err := audit.ContentDigestOf(nested)
	if err != nil {
		t.Fatalf("ContentDigestOf() error = %v", err)
	}
	if !audit.VerifyContentDigest(nd, nn, nested2) {
		t.Error("a typed credential's field value influenced the commitment")
	}
	// The nonce is what protects it: a holder of the commitment without the nonce cannot confirm a
	// guessed body, so a wrong nonce never verifies.
	if audit.VerifyContentDigest(nd, "00", nested) {
		t.Error("the commitment verified under the wrong nonce, so it is not actually keyed")
	}
}

// TestContentDigestOfNoBodyIsAbsent proves a request with no body carries no digest, distinct from a
// request whose body is the empty JSON object.
func TestContentDigestOfNoBodyIsAbsent(t *testing.T) {
	t.Parallel()
	if d, n, err := audit.ContentDigestOf(nil); d != "" || n != "" || err != nil {
		t.Errorf("no body digested to %q/%q (err %v), want empty so absence is distinguishable", d, n, err)
	}
	if d, _, err := audit.ContentDigestOf([]byte(`{}`)); err != nil || d == "" {
		t.Error("an empty JSON object should still carry a digest, distinct from no body at all")
	}
}

// TestContentDigestCanonicalizes proves two semantically identical bodies that differ only in key
// order and whitespace digest the same, so an auditor compares content rather than formatting.
func TestContentDigestCanonicalizes(t *testing.T) {
	t.Parallel()
	a := []byte(`{"name":"a","kind":"env"}`)
	b := []byte("{  \"kind\":\"env\",\n  \"name\":\"a\"  }")
	digest, nonce, err := audit.ContentDigestOf(a)
	if err != nil {
		t.Fatalf("ContentDigestOf() error = %v", err)
	}
	if !audit.VerifyContentDigest(digest, nonce, b) {
		t.Error("key order or whitespace changed the commitment; it is not canonical")
	}
}
