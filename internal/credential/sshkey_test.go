package credential_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/kordloom/switchtender/internal/credential"
)

// newEncryptedKey generates an ed25519 private key sealed in the OpenSSH format under passphrase and
// returns its PEM text, so a test exercises the real unlock path rather than a fixture.
func newEncryptedKey(t *testing.T, passphrase string) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	if err != nil {
		t.Fatalf("marshal encrypted key: %v", err)
	}
	return string(pem.EncodeToMemory(block))
}

func TestParseSSHKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		In      string
		WantKey string
		WantPas string
	}{{ // Test 0: Raw PEM is a bare key with no passphrase.
		Name: "raw pem", In: "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----",
		WantKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----",
	}, { // Test 1: JSON object carries a key and a passphrase.
		Name: "structured", In: `{"private_key":"KEYBODY","passphrase":"s3cret"}`,
		WantKey: "KEYBODY", WantPas: "s3cret",
	}, { // Test 2: JSON without a private_key field is treated as a raw key, not a structured secret.
		Name: "json without key", In: `{"other":"x"}`, WantKey: `{"other":"x"}`,
	}, { // Test 3: Plain text that is not JSON is a raw key.
		Name: "plain text", In: "not-json", WantKey: "not-json",
	}}
	for i, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", i, test.Name), func(t *testing.T) {
			t.Parallel()
			got := credential.ParseSSHKey(test.In)
			if got.PrivateKey != test.WantKey || got.Passphrase != test.WantPas {
				t.Errorf("ParseSSHKey() = {%q, %q}, want {%q, %q}",
					got.PrivateKey, got.Passphrase, test.WantKey, test.WantPas)
			}
		})
	}
}

func TestBuildSSHKeySecretRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Key        string
		Passphrase string
		WantRaw    bool
	}{{ // Test 0: No passphrase seals the raw key unchanged.
		Name: "no passphrase", Key: "-----BEGIN KEY-----\nx", Passphrase: "", WantRaw: true,
	}, { // Test 1: A passphrase seals a structured secret that decodes back to both fields.
		Name: "with passphrase", Key: "-----BEGIN KEY-----\nx", Passphrase: "p:a=s\nw0rd", WantRaw: false,
	}}
	for i, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", i, test.Name), func(t *testing.T) {
			t.Parallel()
			sealed := credential.BuildSSHKeySecret(test.Key, test.Passphrase)
			if test.WantRaw && sealed != test.Key {
				t.Fatalf("BuildSSHKeySecret() = %q, want the raw key %q", sealed, test.Key)
			}
			got := credential.ParseSSHKey(sealed)
			if got.PrivateKey != test.Key || got.Passphrase != test.Passphrase {
				t.Errorf("round trip = {%q, %q}, want {%q, %q}",
					got.PrivateKey, got.Passphrase, test.Key, test.Passphrase)
			}
		})
	}
}

func TestUnlockSSHKey(t *testing.T) {
	t.Parallel()

	// Test 0: The right passphrase yields an unencrypted key that parses with no passphrase.
	t.Run("correct passphrase", func(t *testing.T) {
		t.Parallel()
		encrypted := newEncryptedKey(t, "correct-horse")
		unlocked, err := credential.UnlockSSHKey(encrypted, "correct-horse")
		if err != nil {
			t.Fatalf("UnlockSSHKey() error = %v", err)
		}
		if _, err := ssh.ParseRawPrivateKey([]byte(unlocked)); err != nil {
			t.Errorf("unlocked key does not parse without a passphrase: %v", err)
		}
	})

	// Test 1: A wrong passphrase reports ErrUnlock rather than a decrypted key.
	t.Run("wrong passphrase", func(t *testing.T) {
		t.Parallel()
		encrypted := newEncryptedKey(t, "correct-horse")
		if _, err := credential.UnlockSSHKey(encrypted, "wrong"); !errors.Is(err, credential.ErrUnlock) {
			t.Errorf("UnlockSSHKey() error = %v, want ErrUnlock", err)
		}
	})

	// Test 2: An empty passphrase returns the input unchanged, the unencrypted-key path.
	t.Run("no passphrase passthrough", func(t *testing.T) {
		t.Parallel()
		raw := "-----BEGIN OPENSSH PRIVATE KEY-----\nunencrypted\n-----END OPENSSH PRIVATE KEY-----"
		got, err := credential.UnlockSSHKey(raw, "")
		if err != nil {
			t.Fatalf("UnlockSSHKey() error = %v", err)
		}
		if got != raw {
			t.Errorf("UnlockSSHKey() = %q, want the input unchanged", got)
		}
	})
}
