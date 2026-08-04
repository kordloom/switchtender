package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/witness"
)

// writeAttestation signs an attestation with a fresh identity and writes it, returning the path
// and the signer's key.
func writeAttestation(t *testing.T, mutate func(*witness.Attestation)) (path, signer string) {
	t.Helper()
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	a := &witness.Attestation{
		Server: "https://st.example", MintedAt: time.Now().UTC(),
		CheckpointAt: time.Now().UTC(), LastBeat: 7, LastSeq: 70, LastHead: "aa",
		BeatsRemembered: 7, FindingsTotal: 2,
	}
	if err := witness.SignAttestation(a, id); err != nil {
		t.Fatalf("SignAttestation() error = %v", err)
	}
	if mutate != nil {
		mutate(a)
	}
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	path = filepath.Join(t.TempDir(), "attestation.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path, id.PublicKeyHex()
}

func TestWitnessVerifyAttestation(t *testing.T) {
	good, signer := writeAttestation(t, nil)
	tampered, _ := writeAttestation(t, func(a *witness.Attestation) { a.FindingsTotal = 0 })

	tests := []struct {
		// Name says what the case proves.
		Name string
		// Path is the attestation argument.
		Path string
		// Pin is the --pubkey value.
		Pin string
		// WantErr is whether verification must fail.
		WantErr bool
	}{{ // Test 0: A signed attestation with the right pin verifies.
		Name: "right pin", Path: good, Pin: signer,
	}, { // Test 1: Without a pin the document is only checked for internal consistency.
		Name: "no pin", Path: good,
	}, { // Test 2: The pin is the trust decision; a different key is refused even when the
		// signature itself is sound.
		Name: "wrong pin", Path: good, Pin: "deadbeef", WantErr: true,
	}, { // Test 3: An altered field breaks the signature.
		Name: "tampered", Path: tampered, WantErr: true,
	}, { // Test 4: A missing file is an error, not a pass.
		Name: "missing", Path: filepath.Join(t.TempDir(), "nope.json"), WantErr: true,
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			witnessVerifyPubkey = test.Pin
			defer func() { witnessVerifyPubkey = "" }()
			err := runWitnessVerify(testCommand(), []string{test.Path})
			if (err != nil) != test.WantErr {
				t.Errorf("runWitnessVerify() error = %v, want error %v", err, test.WantErr)
			}
		})
	}
}

func TestWitnessServeRefusesBadFlags(t *testing.T) {
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Watch is the --watch list.
		Watch []string
		// Interval is the --interval value.
		Interval time.Duration
	}{{ // Test 0: No servers to watch is a misconfiguration, not an idle witness.
		Name: "no watch", Interval: time.Minute,
	}, { // Test 1: A watch target must be a web URL.
		Name: "bad scheme", Watch: []string{"ftp://st.example"}, Interval: time.Minute,
	}, { // Test 2: An interval under the floor would hammer the watched servers.
		Name: "short interval", Watch: []string{"https://st.example"}, Interval: time.Second,
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			witnessWatch, witnessServeInterval = test.Watch, test.Interval
			defer func() { witnessWatch, witnessServeInterval = nil, time.Minute }()
			if err := runWitnessServe(testCommand(), nil); err == nil {
				t.Error("runWitnessServe() = nil error, want the misconfiguration refused")
			}
		})
	}
}
