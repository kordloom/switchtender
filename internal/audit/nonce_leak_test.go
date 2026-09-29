package audit

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestNonceNeverLeavesTheServer proves the nonce that keys the content digest is never serialized
// into anything that leaves the machine.
//
// The whole point of keying the digest is that a party holding an export, a SIEM event, or a bundle
// has the commitment but not the key, so they cannot confirm a guessed payload. If the nonce rode
// along in any of those, the keying would be theater. The digest, which is the commitment, must
// travel; the nonce, which is the key, must not.
func TestNonceNeverLeavesTheServer(t *testing.T) {
	t.Parallel()
	digest, nonce, err := ContentDigestOf([]byte(`{"limit":"web01","password":"hunter2"}`))
	if err != nil {
		t.Fatalf("ContentDigestOf() error = %v", err)
	}
	if nonce == "" {
		t.Fatal("no nonce was produced")
	}
	entry := &Entry{
		ID: "aud_1", At: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC), Actor: "deploy-bot",
		ActorType: "agent", OnBehalfOf: "owner", Method: "POST", Path: "/v1/runs",
		ContentDigest: digest, Nonce: nonce,
	}

	// Every serialization of an entry, which is what the SIEM forward and the evidence documents do,
	// must carry the commitment and omit the key.
	marshaled, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("Marshal(entry) error = %v", err)
	}
	if strings.Contains(string(marshaled), nonce) {
		t.Errorf("the nonce is present in a marshaled entry, so it leaves in a SIEM forward:\n%s", marshaled)
	}
	if !strings.Contains(string(marshaled), digest) {
		t.Error("the content digest is absent from a marshaled entry, so the commitment does not travel")
	}

	// The signed bundle, the artifact handed to a third party, must also carry the commitment and not
	// the key.
	id, err := LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	// Link fills the chain fields and hash so the bundle builder accepts it as a genuine genesis
	// entry rather than refusing an unverifiable chain.
	Link(nil, entry)
	bundle, err := BuildBundle([]*Entry{entry}, id, "v1", entry.At)
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("Marshal(bundle) error = %v", err)
	}
	if strings.Contains(string(raw), nonce) {
		t.Errorf("the nonce is present in the signed bundle, so the key leaves with the commitment:\n%s", raw)
	}
	if !strings.Contains(string(raw), digest) {
		t.Error("the content digest is absent from the bundle, so a third party cannot see the commitment")
	}
}
