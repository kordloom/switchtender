package audit

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/kordloom/loomseal/seal"
)

// IdentityFile is the file name the producer identity is stored under, inside the state directory
// beside the database.
const IdentityFile = "producer-key.json"

// Identity is the install's signing identity: the key that signs a bundle and the install id that
// distinguishes this install from another running the same product.
//
// The key is generated once and never leaves the install. It is held in a file rather than in the
// environment so it is not visible in a process listing, a systemd unit, or a container inspect, and
// so an install that was never configured by hand still has an identity rather than silently
// emitting unsigned exports.
//
// It is deliberately not derived from the encryption key. Those two secrets fail differently:
// rotating an encryption key is routine, and if it also rotated the signing identity, every bundle
// already handed to an auditor would stop being attributable to this install.
type Identity struct {
	// InstallID identifies this install in a bundle's producer member.
	InstallID string `json:"install_id"`
	// Seed is the hex ed25519 seed. Hex matches the encoding the existing signed export and the
	// audit verify command already use for keys. It never serializes: writing the key file goes
	// through storedIdentity, which names the seed deliberately, so no other encoder can emit it.
	Seed string `json:"-"`

	// priv is the parsed private key, not serialized.
	priv ed25519.PrivateKey
}

// storedIdentity is the on-disk form of an Identity, and the only place the signing seed is
// encoded.
//
// The seed used to carry a json tag on Identity itself, which made every handler holding an
// identity one careless respondJSON away from publishing the install's private signing key on an
// unauthenticated route. Nothing did that, and the trust handler is careful, but the whole value of
// this key is that it never leaves the install, and a struct that serializes it by default puts
// that one edit away at all times. Writing through a separate type means the seed is emitted only
// where a reader can see it was meant to be.
type storedIdentity struct {
	// InstallID identifies this install in a bundle's producer member.
	InstallID string `json:"install_id"`
	// Seed is the hex ed25519 seed.
	Seed string `json:"seed"`
}

// Public returns the identity's public key, the value a relying party pins by fingerprint.
func (i Identity) Public() ed25519.PublicKey {
	return i.priv.Public().(ed25519.PublicKey)
}

// KeyID returns the sha256 fingerprint of the public key, the value to publish on a trust page.
func (i Identity) KeyID() string { return seal.KeyID(i.Public()) }

// Private returns the signing key.
func (i Identity) Private() ed25519.PrivateKey { return i.priv }

// PublicKeyHex returns the hex public key, matching what audit verify --pubkey expects.
func (i Identity) PublicKeyHex() string { return hex.EncodeToString(i.Public()) }

// PublicKeyBase64 returns the standard base64 public key, the encoding a bundle carries. The hex and
// base64 forms are the same key and the same trust, in the encoding each envelope calls for.
func (i Identity) PublicKeyBase64() string {
	return base64.StdEncoding.EncodeToString(i.Public())
}

// LoadIdentity reads the producer identity from dir, creating it on first use.
//
// An operator who would rather hold the key in their own secret manager sets SWITCHTENDER_AUDIT_KEY
// to a hex seed and it wins, so the file is a default rather than a requirement. That environment
// variable is the same one the existing signed export already uses, so an install that has one keeps
// the identity it has been signing with.
func LoadIdentity(dir string) (Identity, error) {
	if seed := os.Getenv("SWITCHTENDER_AUDIT_KEY"); seed != "" {
		id, err := identityFromSeed(seed, "")
		if err != nil {
			return Identity{}, fmt.Errorf("SWITCHTENDER_AUDIT_KEY: %w", err)
		}
		// The install id is derived from the key that signs, never taken from a file written for a
		// different key. Reading it from the file meant an operator who set this variable over an
		// existing install emitted bundles signed by one key and attributed to another install, and
		// the documented relationship between the id and the key silently stopped holding.
		id.InstallID = installIDFromKey(id.Public())
		return id, nil
	}

	stored, err := readIdentityFile(dir)
	if err == nil {
		return identityFromSeed(stored.Seed, stored.InstallID)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return Identity{}, err
	}
	return createIdentity(dir)
}

// readIdentityFile reads the stored identity, returning fs.ErrNotExist when there is none.
func readIdentityFile(dir string) (Identity, error) {
	raw, err := os.ReadFile(filepath.Join(dir, IdentityFile))
	if err != nil {
		return Identity{}, err
	}
	var stored storedIdentity
	if err := json.Unmarshal(raw, &stored); err != nil {
		return Identity{}, fmt.Errorf("producer identity: parse %s: %w", IdentityFile, err)
	}
	return Identity{InstallID: stored.InstallID, Seed: stored.Seed}, nil
}

// createIdentity generates a new identity and writes it to dir with owner-only permissions.
func createIdentity(dir string) (Identity, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Identity{}, fmt.Errorf("producer identity: create %s: %w", dir, err)
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return Identity{}, fmt.Errorf("producer identity: generate key: %w", err)
	}
	id, err := identityFromSeed(hex.EncodeToString(seed), "")
	if err != nil {
		return Identity{}, err
	}
	id.InstallID = installIDFromKey(id.Public())

	raw, err := json.MarshalIndent(storedIdentity{InstallID: id.InstallID, Seed: id.Seed}, "", "  ")
	if err != nil {
		return Identity{}, fmt.Errorf("producer identity: encode: %w", err)
	}
	path := filepath.Join(dir, IdentityFile)
	// Written through a uniquely named temporary file so a crash cannot leave a half-written key and
	// so two processes starting at once cannot overwrite each other's, and at 0600 so the signing
	// seed is readable only by the account running the server.
	f, err := os.CreateTemp(dir, IdentityFile+".*.tmp")
	if err != nil {
		return Identity{}, fmt.Errorf("producer identity: write: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return Identity{}, fmt.Errorf("producer identity: protect: %w", err)
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		_ = f.Close()
		return Identity{}, fmt.Errorf("producer identity: write: %w", err)
	}
	if err := f.Close(); err != nil {
		return Identity{}, fmt.Errorf("producer identity: write: %w", err)
	}
	// Link rather than rename, so the first process to finish wins and the rest adopt its key
	// instead of each believing its own. An install has one identity; two would mean two producers
	// claiming to be the same install.
	if err := os.Link(tmp, path); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return Identity{}, fmt.Errorf("producer identity: install: %w", err)
		}
		winner, readErr := readIdentityFile(dir)
		if readErr != nil {
			return Identity{}, fmt.Errorf("producer identity: adopt: %w", readErr)
		}
		return identityFromSeed(winner.Seed, winner.InstallID)
	}
	return id, nil
}

// identityFromSeed parses a hex seed into a usable identity.
func identityFromSeed(hexSeed, installID string) (Identity, error) {
	seed, err := hex.DecodeString(hexSeed)
	if err != nil {
		return Identity{}, fmt.Errorf("producer identity: decode seed: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return Identity{}, fmt.Errorf("producer identity: seed must be %d bytes, got %d",
			ed25519.SeedSize, len(seed))
	}
	id := Identity{InstallID: installID, Seed: hexSeed, priv: ed25519.NewKeyFromSeed(seed)}
	if id.InstallID == "" {
		id.InstallID = installIDFromKey(id.Public())
	}
	return id, nil
}

// installIDFromKey derives a stable install id from the public key, so the id needs no separate
// management and always corresponds to the key that signs the bundles carrying it.
func installIDFromKey(pub ed25519.PublicKey) string {
	return "in_" + hex.EncodeToString(pub[:6])
}
