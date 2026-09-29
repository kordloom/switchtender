package dispatch

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/kordloom/switchtender/internal/credential"
)

// TestUnlockedSSHKeyIsMasked checks that the decrypted form of a passphrase-protected key is
// redacted from run output, not only the stored form and the passphrase.
//
// A bare key was masked because the stored value is the key itself. A passphrase-protected one is
// stored as JSON, where the PEM line breaks are escaped, so the masker never saw a line it could
// match. Unlocking then re-encodes the key, so what lands on disk is not the bytes anyone
// registered. A playbook that reads the key file back wrote the usable private key verbatim into the
// stored log, which any viewer of that run can fetch.
func TestUnlockedSSHKeyIsMasked(t *testing.T) {
	t.Parallel()
	const passphrase = "correct-horse"
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	if err != nil {
		t.Fatalf("marshal encrypted key: %v", err)
	}
	encrypted := string(pem.EncodeToMemory(block))

	// This is how a passphrase-protected key is stored, and what used to be the only thing masked.
	stored, err := json.Marshal(map[string]string{
		"private_key": encrypted, "passphrase": passphrase,
	})
	if err != nil {
		t.Fatalf("marshal stored form: %v", err)
	}
	material := credential.ParseSSHKey(string(stored))
	unlocked, err := credential.UnlockSSHKey(material.PrivateKey, material.Passphrase)
	if err != nil {
		t.Fatalf("UnlockSSHKey() error = %v", err)
	}

	// Registering only the stored form and the passphrase, the way it used to be.
	old := &masker{}
	old.set([]string{string(stored), passphrase})
	if !strings.Contains(string(old.redact([]byte(unlocked))), "PRIVATE KEY") {
		t.Skip("the stored form already covers the unlocked key, so this no longer applies")
	}

	// Registering the unlocked key too, the way it is now.
	m := &masker{}
	m.set([]string{string(stored), passphrase, unlocked})
	got := string(m.redact([]byte(unlocked)))
	if strings.Contains(got, "PRIVATE KEY") {
		t.Errorf("a playbook reading the key file writes it into the stored log: %q",
			got[:min(len(got), 120)])
	}
	if !strings.Contains(string(m.redact([]byte("pass="+passphrase))), "pass=") {
		t.Error("the passphrase is no longer masked")
	}
}
