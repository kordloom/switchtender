package audit

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// TestStoredTimestampProofIsCheckedNotAssumed covers the gap between what a timestamp anchor claims
// and what anything checked. The token is verified once, when it is obtained, and then stored. Nothing
// read it again: a verifier reported the anchor as satisfied because the chain reached the recorded
// link, and the dossier told the auditor the anchor "verifies offline" because a proof string was
// present. So the third-party part of the claim rested on our own database row. Edit the proof column,
// or the link it is supposed to commit to, and the artifact still read as timestamped by an authority.
func TestStoredTimestampProofIsCheckedNotAssumed(t *testing.T) {
	t.Parallel()
	// A real token over a known link, produced by the same encoder the timestamp request uses, so this
	// test needs no network and no authority.
	link := strings.Repeat("ab", 32)
	raw, err := hex.DecodeString(link)
	if err != nil {
		t.Fatalf("decode link: %v", err)
	}
	sum := sha256.Sum256(raw)
	token := base64.StdEncoding.EncodeToString(tokenOver(t, sum[:]))

	tests := []struct {
		Name    string
		Link    string
		Proof   string
		WantErr string
	}{ // Test 0: The token over this link verifies.
		{"matching token", link, token, ""},
		// Test 1: The same token against a different link says nothing about that link, which is what a
		// rewritten anchor row looks like.
		{"token for another link", strings.Repeat("cd", 32), token, "different value"},
		// Test 2: A proof that is not a token at all was reported as one.
		{"not a token", link, base64.StdEncoding.EncodeToString([]byte("trust me")), "decode"},
		// Test 3: A proof that is not even base64.
		{"not base64", link, "!!!!", "base64"},
		// Test 4: An anchor with no embedded proof is not an offline anchor, and saying so is not an
		// error: it is the ordinary git or https anchor.
		{"no proof", link, "", "no embedded proof"},
	}
	for i, tc := range tests {
		err := VerifyTimestampProof(tc.Link, tc.Proof)
		if tc.WantErr == "" {
			if err != nil {
				t.Errorf("test %d (%s): VerifyTimestampProof = %v, want it verified", i, tc.Name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("test %d (%s): VerifyTimestampProof = nil, want a refusal", i, tc.Name)
			continue
		}
		if !strings.Contains(err.Error(), tc.WantErr) {
			t.Errorf("test %d (%s): error = %v, want it to mention %q", i, tc.Name, err, tc.WantErr)
		}
	}
}

// TestVerifyBundleRefusesABadTimestampToken proves the check reaches the verifier, not only the helper.
// A bundle carries its anchors, so a receipt handed to a third party can carry a timestamp token that
// commits to something other than the link it sits beside. Reporting the anchors as satisfied because the
// chain reached that link, without reading the token, is what let the producer's own row stand in for the
// authority's statement.
func TestVerifyBundleRefusesABadTimestampToken(t *testing.T) {
	// No t.Parallel: this sets an environment variable, so it must not run beside another test.
	entries := buildChain(t, 3)
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	doc, berr := BuildBundle(entries, id, "test", time.Now())
	if berr != nil {
		t.Fatalf("BuildBundle: %v", berr)
	}
	head := entries[len(entries)-1]

	// A token over a different link than the anchor names, which is what an edited anchors table looks
	// like from the outside.
	otherLink := strings.Repeat("cd", 32)
	raw, derr := hex.DecodeString(otherLink)
	if derr != nil {
		t.Fatalf("decode: %v", derr)
	}
	sum := sha256.Sum256(raw)
	doc.AttachAnchors([]*Anchor{{
		ID: "anc_1", Type: AnchorRFC3161, Shape: AnchorShapeLinear, Seq: head.Seq, Link: head.Hash,
		At: time.Now(), Ref: "https://tsa.example.com",
		Proof: base64.StdEncoding.EncodeToString(tokenOver(t, sum[:])),
	}})
	signed, serr := SignBundleDoc(doc, id.Private())
	if serr != nil {
		t.Fatalf("SignBundleDoc: %v", serr)
	}

	rep, verr := VerifyBundle(signed, "")
	if verr != nil {
		t.Fatalf("VerifyBundle: %v", verr)
	}
	if rep.AnchorsOK {
		t.Error("a bundle whose timestamp token fixes a different link verified its anchors, so the " +
			"authority's statement was never read")
	}
	if rep.OK() {
		t.Error("the bundle verified overall despite an anchor proof that says nothing about it")
	}
	if len(rep.TimestampProblems) == 0 {
		t.Error("the report does not say which anchor's token was refused")
	}
}
