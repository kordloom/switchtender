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

	// Registering the unlocked key too, the way it is now. This is checked first and
	// unconditionally. It sat behind a skip on the comparison below, so the day the old shape
	// stopped being worse, the product's own property would have stopped being checked along with
	// the point being made about it.
	m := &masker{}
	m.set([]string{string(stored), passphrase, unlocked})
	got := string(m.redact([]byte(unlocked)))
	if strings.Contains(got, "PRIVATE KEY") {
		t.Errorf("a playbook reading the key file writes it into the stored log: %q",
			got[:min(len(got), 120)])
	}
	// The passphrase itself is what must not survive. Checking that the label around it survived
	// passed whether or not the passphrase was masked.
	if masked := string(m.redact([]byte("pass=" + passphrase))); strings.Contains(masked, passphrase) {
		t.Errorf("the passphrase is no longer masked: %q", masked)
	}

	// Registering only the stored form and the passphrase, the way it used to be, which is the
	// point rather than a requirement: it says why the unlocked key has to be registered at all. If
	// the stored form ever covers the unlocked key on its own, that is worth knowing and is not a
	// failure.
	old := &masker{}
	old.set([]string{string(stored), passphrase})
	if !strings.Contains(string(old.redact([]byte(unlocked))), "PRIVATE KEY") {
		t.Log("the stored form now covers the unlocked key on its own, so registering the unlocked " +
			"key is no longer the only thing standing between a playbook that reads the key file " +
			"and the stored log")
	}
}
