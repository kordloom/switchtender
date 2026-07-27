package credential

import (
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// ErrUnlock is returned when a passphrase cannot decrypt an SSH private key, whether the passphrase
// is wrong or the key format is unsupported.
var ErrUnlock = errors.New("ssh key unlock failed")

// SSHKeyMaterial is the decoded content of an ssh_key credential's plaintext: a PEM private key and
// an optional passphrase that decrypts it.
type SSHKeyMaterial struct {
	// PrivateKey is the PEM encoded private key.
	PrivateKey string `json:"private_key"`
	// Passphrase decrypts the private key, empty when the key is not encrypted.
	Passphrase string `json:"passphrase,omitempty"`
}

// ParseSSHKey decodes an ssh_key credential's plaintext. A plaintext that is a JSON object carrying a
// private_key field is a structured secret with an optional passphrase; any other plaintext is a raw
// private key with no passphrase, which is the zero-config form and keeps keys stored before this
// format working unchanged. A private key is PEM and starts with a dashed header, never a brace, so
// the discriminator is unambiguous.
func ParseSSHKey(plain string) SSHKeyMaterial {
	if strings.HasPrefix(strings.TrimSpace(plain), "{") {
		var m SSHKeyMaterial
		if err := json.Unmarshal([]byte(plain), &m); err == nil && m.PrivateKey != "" {
			return m
		}
	}
	return SSHKeyMaterial{PrivateKey: plain}
}

// BuildSSHKeySecret returns the plaintext to seal for an ssh_key credential. Without a passphrase it
// returns the raw key unchanged, the zero-config form; with a passphrase it returns a JSON object so
// the passphrase travels sealed alongside the key inside the single stored secret, needing no second
// column and no schema change.
func BuildSSHKeySecret(privateKey, passphrase string) string {
	if passphrase == "" {
		return privateKey
	}
	data, err := json.Marshal(SSHKeyMaterial{PrivateKey: privateKey, Passphrase: passphrase})
	if err != nil {
		// A struct of two strings cannot fail to marshal; a failure here is a developer error.
		panic("credential: marshal ssh key material: " + err.Error())
	}
	return string(data)
}

// UnlockSSHKey decrypts a passphrase protected private key and returns an unencrypted OpenSSH PEM that
// any tool consumes without an interactive prompt. A key with an empty passphrase is returned
// unchanged, so an unencrypted key passes straight through. Decryption happens in process, so the
// passphrase never reaches a command line, a temp file, or an agent.
func UnlockSSHKey(privateKey, passphrase string) (string, error) {
	if passphrase == "" {
		return privateKey, nil
	}
	key, err := ssh.ParseRawPrivateKeyWithPassphrase([]byte(privateKey), []byte(passphrase))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnlock, err)
	}
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnlock, err)
	}
	return string(pem.EncodeToMemory(block)), nil
}
