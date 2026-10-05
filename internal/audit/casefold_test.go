package audit

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kordloom/loomseal/seal"
)

// buildSignedLinear builds a valid, signed, linear bundle and returns it with the identity that
// signed it, so a test can mutate the signed bytes and re-sign them as the same producer.
func buildSignedLinear(t *testing.T) ([]byte, Identity) {
	t.Helper()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	entries := buildChain(t, 3)
	var prev *Entry
	for _, e := range entries {
		e.InstallID = id.InstallID
		Link(prev, e)
		prev = e
	}
	doc, err := BuildBundle(entries, id, "1.101.0", time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	signed, err := SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}
	return signed, id
}

// TestVerifyBundleRefusesCaseVariantMembers is the guarantee: a member a verdict reads is matched by
// its exact name, so a case variant of a known member is refused rather than folded onto the struct
// field. Without this a claim carrying both at and At, or a producer carrying install_id and
// Install_ID, would decode the variant into the field this verifier reads while a reader and the leaf
// saw the exact member, and the two could differ on the same signed bytes.
func TestVerifyBundleRefusesCaseVariantMembers(t *testing.T) {
	signed, id := buildSignedLinear(t)

	// The negative control: the un-mutated bundle verifies, so the member check rejects only the
	// planted variants and not every clean bundle.
	rep, err := VerifyBundle(signed, id.KeyID())
	if err != nil || rep == nil || !rep.SignatureOK || !rep.ChainOK {
		t.Fatalf("control bundle did not verify: err=%v report=%+v", err, rep)
	}

	// resign mutates the parsed bundle and re-signs it as the same producer, so the only fault the
	// verifier can find is the planted member. The signatures array is re-created by signing, so a
	// variant planted inside a signature is applied without re-signing instead.
	resign := func(mutate func(map[string]any)) []byte {
		var m map[string]any
		if err := json.Unmarshal(signed, &m); err != nil {
			t.Fatal(err)
		}
		mutate(m)
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		out, err := seal.SignBundle(raw, id.Private())
		if err != nil {
			t.Fatalf("re-sign: %v", err)
		}
		return out
	}
	mutateOnly := func(mutate func(map[string]any)) []byte {
		var m map[string]any
		if err := json.Unmarshal(signed, &m); err != nil {
			t.Fatal(err)
		}
		mutate(m)
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	firstClaim := func(m map[string]any) map[string]any {
		return m["claims"].([]any)[0].(map[string]any)
	}
	tests := []struct {
		Name string
		Bad  []byte
	}{
		{Name: "producer install_id", Bad: resign(func(m map[string]any) {
			m["producer"].(map[string]any)["Install_ID"] = "in_elsewhere"
		})},
		{Name: "bundle_id", Bad: resign(func(m map[string]any) {
			m["Bundle_ID"] = "lsb_elsewhere"
		})},
		{Name: "claim at", Bad: resign(func(m map[string]any) {
			firstClaim(m)["At"] = "2020-01-01T00:00:00Z"
		})},
		{Name: "claim chain seq", Bad: resign(func(m map[string]any) {
			firstClaim(m)["chain"].(map[string]any)["Seq"] = float64(99)
		})},
		{Name: "signature key_id", Bad: mutateOnly(func(m map[string]any) {
			sig := m["signatures"].([]any)[0].(map[string]any)
			sig["Key_ID"] = sig["key_id"]
		})},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			if _, err := VerifyBundle(test.Bad, id.KeyID()); err == nil {
				t.Errorf("a case variant of %s verified rather than being refused", test.Name)
			}
		})
	}
}
