// Package credential stores execution secrets, SSH private keys and vault passwords, encrypted at
// rest with AES-GCM. Secret material decrypts only at execution time, never leaves the process in
// API responses, and is wiped from temp files when the run finishes.
package credential

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

// Kind classifies what a credential holds and how the runner materializes it.
type Kind string

const (
	// KindSSHKey is an SSH private key handed to ansible-playbook as --private-key.
	KindSSHKey Kind = "ssh_key"
	// KindVaultPassword is an Ansible Vault password handed over as --vault-password-file.
	KindVaultPassword Kind = "vault_password"
)

var (
	// ErrNotFound is returned when a credential does not exist in the store.
	ErrNotFound = errors.New("credential not found")
	// ErrBadKind is returned when a credential kind is not recognized.
	ErrBadKind = errors.New("unknown credential kind")
	// ErrNoKey is returned when encryption is attempted without an encryption key configured.
	ErrNoKey = errors.New("no encryption key: set YARDMASTER_ENCRYPTION_KEY")
)

// ValidKind reports whether k names a supported credential kind.
func ValidKind(k Kind) bool {
	return k == KindSSHKey || k == KindVaultPassword
}

// Credential is one stored secret. Secret carries ciphertext inside the store and plaintext only
// in the hands of the executor; it never serializes to JSON.
type Credential struct {
	// ID is the unique credential identifier.
	ID string `json:"id"`
	// Name labels the credential for humans, for example prod-fleet-key.
	Name string `json:"name"`
	// Kind classifies the secret.
	Kind Kind `json:"kind"`
	// Secret is the encrypted material at rest and never appears in API responses.
	Secret string `json:"-"`
	// CreatedAt is when the credential was created.
	CreatedAt time.Time `json:"created_at"`
}

// Store persists credentials with their secrets encrypted by the caller. Implementations must be
// safe for concurrent use.
type Store interface {
	// Save inserts or replaces the credential identified by c.ID.
	Save(ctx context.Context, c *Credential) error
	// Get returns the credential with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*Credential, error)
	// List returns all credentials ordered by creation time, oldest first.
	List(ctx context.Context) ([]*Credential, error)
	// Delete removes the credential with the given id, or returns ErrNotFound.
	Delete(ctx context.Context, id string) error
}

// NewID returns a random credential identifier prefixed with "cred_".
func NewID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("credential: read random: " + err.Error())
	}
	return "cred_" + hex.EncodeToString(b[:])
}
