// Package credential stores execution secrets, SSH private keys and vault passwords, encrypted at
// rest with AES-GCM. Secret material decrypts only at execution time, never leaves the process in
// API responses, and is wiped from temp files when the run finishes.
package credential

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/dcadolph/yardmaster/internal/secretsource"
)

// Kind classifies what a credential holds and how the runner materializes it.
type Kind string

const (
	// KindSSHKey is an SSH private key handed to ansible-playbook as --private-key.
	KindSSHKey Kind = "ssh_key"
	// KindVaultPassword is an Ansible Vault password handed over as --vault-password-file.
	KindVaultPassword Kind = "vault_password"
	// KindEnv is newline separated KEY=VALUE pairs injected into the execution environment,
	// which is how cloud SDK credentials reach inventory plugins and playbook modules.
	KindEnv Kind = "env"
	// KindBecomePassword is a privilege escalation password handed to a run as the
	// ansible_become_password variable through a file, so it never appears on the command line.
	KindBecomePassword Kind = "become_password"
	// KindRegistry is a container registry username and password, the username on the first line
	// and the password on the rest, used to pull execution environment images.
	KindRegistry Kind = "registry"
	// KindToken is a single API token or JWT, exposed to a run as the TokenEnvVar environment
	// variable so any tool can send it as a bearer token without a KEY=VALUE wrapper.
	KindToken Kind = "token"
)

// TokenEnvVar is the environment variable a token credential is exposed under at run time.
const TokenEnvVar = "YARDMASTER_TOKEN"

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
	switch k {
	case KindSSHKey, KindVaultPassword, KindEnv, KindBecomePassword, KindRegistry, KindToken:
		return true
	default:
		return false
	}
}

const (
	// SourceLocal means the sealed Secret is the value itself. It is the default.
	SourceLocal = secretsource.KindLocal
	// SourceCommand means the sealed Secret is a command whose stdout is the secret.
	SourceCommand = secretsource.KindCommand
	// SourceVault means the sealed Secret is a Vault config read over HTTP at run time.
	SourceVault = secretsource.KindVault
	// SourceGSM means the sealed Secret is a Google Secret Manager config read over HTTP at run time.
	SourceGSM = secretsource.KindGSM
	// SourceVaultDynamic means the sealed Secret is a Vault dynamic secrets config. A short-lived
	// credential is minted for each run and revoked when the run ends.
	SourceVaultDynamic = secretsource.KindVaultDynamic
)

// NormalizeSource maps an empty source to the local default and otherwise returns source unchanged.
func NormalizeSource(source string) string { return secretsource.NormalizeKind(source) }

// ValidSource reports whether s names a supported credential source. Empty is valid and means local.
func ValidSource(s string) bool { return secretsource.ValidKind(s) }

// RegistryLogin splits registry credential material into its username and password. The first line
// is the username; everything after it is the password, so a password may contain any character.
func RegistryLogin(secret string) (username, password string) {
	username, password, found := strings.Cut(secret, "\n")
	if !found {
		return strings.TrimSpace(secret), ""
	}
	return strings.TrimSpace(username), password
}

// EnvLines splits env credential material into KEY=VALUE entries, dropping blanks and comments.
func EnvLines(secret string) []string {
	var out []string
	for line := range strings.SplitSeq(secret, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		out = append(out, line)
	}
	return out
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
	// Source is where the secret value comes from: empty or local means Secret holds the sealed value
	// itself; command means Secret holds a command whose stdout is the secret, fetched at run time
	// from an external store such as Vault or a cloud CLI.
	Source string `json:"source,omitempty"`
	// Secret is the encrypted material at rest and never appears in API responses. For a command
	// source it is the sealed command, not the secret.
	Secret string `json:"-"`
	// CreatedAt is when the credential was created.
	CreatedAt time.Time `json:"created_at"`
}

// Store persists credentials with their secrets encrypted by the caller. Implementations must be
// safe for concurrent use.
type Store interface {
	// Save inserts or replaces the credential identified by c.ID.
	Save(ctx context.Context, c *Credential) error
	// Update changes an existing credential's name, kind, and sealed secret, preserving its creation
	// time, or returns ErrNotFound.
	Update(ctx context.Context, c *Credential) error
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
