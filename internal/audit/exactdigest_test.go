package audit_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/audit"
)

// TestDigestFormsCheckWhatTheyCommitted pins the three digest forms against the two ways a
// disclosure is checked. VerifyContentDigest reduces what it is handed for the keyed and unkeyed
// forms, as this product always has, and takes the exact form's bytes as given.
// VerifyCanonicalDigest reduces nothing, which is how every LoomSeal verifier checks a disclosed
// record, so a body passes it only when the bytes disclosed are the bytes committed.
func TestDigestFormsCheckWhatTheyCommitted(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"step":"release password=hunter2-digest-secret","verdict":"approved"}`)
	reduced, err := audit.CanonicalRedacted(raw)
	if err != nil {
		t.Fatalf("CanonicalRedacted() error = %v", err)
	}
	keyed, keyedNonce, err := audit.ContentDigestOf(raw)
	if err != nil {
		t.Fatalf("ContentDigestOf() error = %v", err)
	}
	exact, exactNonce, err := audit.ExactDigestOf(reduced)
	if err != nil {
		t.Fatalf("ExactDigestOf() error = %v", err)
	}
	sum := sha256.Sum256(reduced)
	unkeyed := "sha256:" + hex.EncodeToString(sum[:])

	tests := []struct {
		Digest        string
		Nonce         string
		Body          []byte
		WantContent   bool
		WantCanonical bool
	}{{ // Test 0: The keyed form verifies the assembled body only through this product's redaction.
		Digest: keyed, Nonce: keyedNonce, Body: raw, WantContent: true, WantCanonical: false,
	}, { // Test 1: The keyed form verifies the redacted bytes both ways.
		Digest: keyed, Nonce: keyedNonce, Body: reduced, WantContent: true, WantCanonical: true,
	}, { // Test 2: The exact form verifies the bytes it committed both ways.
		Digest: exact, Nonce: exactNonce, Body: reduced, WantContent: true, WantCanonical: true,
	}, { // Test 3: The exact form does not reduce, so the assembled body fails both ways.
		Digest: exact, Nonce: exactNonce, Body: raw, WantContent: false, WantCanonical: false,
	}, { // Test 4: An altered byte fails the exact form.
		Digest: exact, Nonce: exactNonce, Body: append([]byte(nil), reduced[:len(reduced)-1]...),
		WantContent: false, WantCanonical: false,
	}, { // Test 5: A nonce the exact form did not commit fails.
		Digest: exact, Nonce: keyedNonce, Body: reduced, WantContent: false, WantCanonical: false,
	}, { // Test 6: The unkeyed form verifies the redacted bytes both ways.
		Digest: unkeyed, Body: reduced, WantContent: true, WantCanonical: true,
	}, { // Test 7: The unkeyed form verifies the assembled body only through this product's redaction.
		Digest: unkeyed, Body: raw, WantContent: true, WantCanonical: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := audit.VerifyContentDigest(test.Digest, test.Nonce, test.Body)
			if got != test.WantContent {
				t.Errorf("VerifyContentDigest() = %v, want %v", got, test.WantContent)
			}
			got = audit.VerifyCanonicalDigest(test.Digest, test.Nonce, test.Body)
			if got != test.WantCanonical {
				t.Errorf("VerifyCanonicalDigest() = %v, want %v", got, test.WantCanonical)
			}
		})
	}
	if !strings.HasPrefix(exact, audit.ExactDigestPrefix) {
		t.Errorf("ExactDigestOf() = %q, want the %s form", exact, audit.ExactDigestPrefix)
	}
	if d, n, err := audit.ExactDigestOf(nil); d != "" || n != "" || err != nil {
		t.Errorf("ExactDigestOf(nil) = %q, %q, %v, want no digest for no body", d, n, err)
	}
}
