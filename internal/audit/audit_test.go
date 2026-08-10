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
	a := []byte(`{"name":"prod","kind":"ssh_key","secret":"hunter2","passphrase":"unlock-me"}`)
	b := []byte(`{"name":"prod","kind":"ssh_key","secret":"totally-different","passphrase":"other"}`)
	da, db := audit.ContentDigestOf(a), audit.ContentDigestOf(b)
	if da == "" || da != db {
		t.Errorf("digests differ on secret value alone: %s vs %s; the secret leaked into the digest",
			da, db)
	}
	// A change in a non-secret field must change the digest, or the digest proves nothing about the
	// shape of the change.
	c := []byte(`{"name":"staging","kind":"ssh_key","secret":"hunter2","passphrase":"unlock-me"}`)
	if audit.ContentDigestOf(c) == da {
		t.Error("changing the name did not change the digest, so it commits to nothing useful")
	}
	// A nested secret, as a typed credential's fields object carries, is also redacted.
	nested := []byte(`{"name":"dd","type_id":"ct_1","fields":{"api_key":"sk-live-123"}}`)
	nested2 := []byte(`{"name":"dd","type_id":"ct_1","fields":{"api_key":"sk-live-999"}}`)
	if audit.ContentDigestOf(nested) != audit.ContentDigestOf(nested2) {
		t.Error("a typed credential's field values leaked into the digest")
	}
}

// TestContentDigestOfNoBodyIsAbsent proves a request with no body carries no digest, distinct from a
// request whose body is the empty JSON object.
func TestContentDigestOfNoBodyIsAbsent(t *testing.T) {
	t.Parallel()
	if got := audit.ContentDigestOf(nil); got != "" {
		t.Errorf("no body digested to %q, want empty so absence is distinguishable", got)
	}
	if got := audit.ContentDigestOf([]byte(`{}`)); got == "" {
		t.Error("an empty JSON object should still carry a digest, distinct from no body at all")
	}
}

// TestContentDigestCanonicalizes proves two semantically identical bodies that differ only in key
// order and whitespace digest the same, so an auditor compares content rather than formatting.
func TestContentDigestCanonicalizes(t *testing.T) {
	t.Parallel()
	a := []byte(`{"name":"a","kind":"env"}`)
	b := []byte("{  \"kind\":\"env\",\n  \"name\":\"a\"  }")
	if audit.ContentDigestOf(a) != audit.ContentDigestOf(b) {
		t.Error("key order or whitespace changed the digest; it is not canonical")
	}
}
