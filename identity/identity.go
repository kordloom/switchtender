// Package identity is the install's signing identity: an ed25519 key held in a file, the key that
// signs a bundle and the install id derived from it. It carries no dependency on the rest of the
// product, so a witness or any out-of-tree tool that needs to sign or pin a key builds against it
// alone.
package identity

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

// File is the file name the producer identity is stored under, inside the state directory
// beside the database.
const File = "producer-key.json"

// KeyEnv is the environment variable that supplies the producer seed, for an operator holding the key
// in their own secret manager and for every process that must sign as one shared install.
const KeyEnv = "SWITCHTENDER_AUDIT_KEY"

// WitnessFile is the file name a witness's own signing identity is stored under. A witness must not
// share a file with the server it watches: reading producer-key.json meant a witness pointed at the
// watched server's state directory, which is what the default does when the two run on one host,
// signed its attestations with that server's own key. A relying party pinning it was pinning the
// operator's key, and the operator could then mint the statement the witness exists to make
// unforgeable.
const WitnessFile = "witness-key.json"

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

// Load reads the producer identity from dir, creating it on first use.
//
// An operator who would rather hold the key in their own secret manager sets SWITCHTENDER_AUDIT_KEY
// to a hex seed and it wins, so the file is a default rather than a requirement. That environment
// variable is the same one the existing signed export already uses, so an install that has one keeps
// the identity it has been signing with.
func Load(dir string) (Identity, error) {
	if seed := os.Getenv(KeyEnv); seed != "" {
		id, err := identityFromSeed(seed, "")
		if err != nil {
			return Identity{}, fmt.Errorf("%s: %w", KeyEnv, err)
		}
		// The install id is derived from the key that signs, never taken from a file written for a
		// different key. Reading it from the file meant an operator who set this variable over an
		// existing install emitted bundles signed by one key and attributed to another install, and
		// the documented relationship between the id and the key silently stopped holding.
		id.InstallID = installIDFromKey(id.Public())
		return id, nil
	}

	return LoadFile(dir)
}

// LoadFile reads the identity from dir alone, creating it on first use, and never consults
// SWITCHTENDER_AUDIT_KEY.
//
// A witness needs this. Load lets that variable win so an operator can hold the producer key in
// their own secret manager, which is right for the server that produces a chain and wrong for
// anything meant to be independent of it. A witness started where the variable is set signed its
// attestations with the watched server's key, so a relying party who pinned the witness key was
// pinning the producer's, and the watched operator could mint the very statement the witness exists
// to make unforgeable. It reads the same file under the same name, so a witness that already has a
// key keeps it, along with the checkpoints that key protects.
func LoadFile(dir string) (Identity, error) {
	stored, err := readIdentityFile(dir)
	if err == nil {
		return identityFromSeed(stored.Seed, stored.InstallID)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return Identity{}, err
	}
	return createIdentity(dir)
}

// LoadWitnessFile reads a witness's own identity from dir, creating it on first use, under a file name
// of its own so a witness can never sign with the producer key of the server it watches. Like LoadFile
// it never consults SWITCHTENDER_AUDIT_KEY.
//
// A key that matches a producer identity sitting in the same directory is refused rather than used. That
// only happens if somebody copied one file over the other, and the whole value of a witness signature is
// that it is not the watched party's.
func LoadWitnessFile(dir string) (Identity, error) {
	id, err := loadNamed(dir, WitnessFile)
	if err != nil {
		return Identity{}, err
	}
	producer, perr := readNamed(dir, File)
	if perr == nil && producer.Seed == id.Seed {
		return Identity{}, fmt.Errorf("witness identity: %s in %s holds the same key as %s, so this "+
			"witness would countersign the server it watches: delete it and let a new witness key be "+
			"created", WitnessFile, dir, File)
	}
	return id, nil
}

// readIdentityFile reads the stored producer identity, returning fs.ErrNotExist when there is none.
func readIdentityFile(dir string) (Identity, error) {
	return readNamed(dir, File)
}

// loadNamed reads the identity stored under name in dir, creating it when there is none.
func loadNamed(dir, name string) (Identity, error) {
	stored, err := readNamed(dir, name)
	if err == nil {
		return identityFromSeed(stored.Seed, stored.InstallID)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return Identity{}, err
	}
	return createNamed(dir, name)
}

// readNamed reads one stored identity file, returning fs.ErrNotExist when it is not there.
func readNamed(dir, name string) (Identity, error) {
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return Identity{}, err
	}
	var stored storedIdentity
	if err := json.Unmarshal(raw, &stored); err != nil {
		return Identity{}, fmt.Errorf("identity: parse %s: %w", name, err)
	}
	return Identity{InstallID: stored.InstallID, Seed: stored.Seed}, nil
}

// createIdentity generates a new identity and writes it to dir with owner-only permissions.
func createIdentity(dir string) (Identity, error) {
	return createNamed(dir, File)
}

// createNamed generates an identity and writes it to dir under name with owner-only permissions.
func createNamed(dir, name string) (Identity, error) {
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
	path := filepath.Join(dir, name)
	// Written through a uniquely named temporary file so a crash cannot leave a half-written key and
	// so two processes starting at once cannot overwrite each other's, and at 0600 so the signing
	// seed is readable only by the account running the server.
	f, err := os.CreateTemp(dir, name+".*.tmp")
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

// InstallIDFromKey returns the install id that belongs to a public key. A verifier needs it to check
// that a bundle naming an install was signed by that install's key rather than by whoever re-signed
// it, which is the tie the id alone does not make.
func InstallIDFromKey(pub ed25519.PublicKey) string { return installIDFromKey(pub) }

// installIDFromKey derives a stable install id from the public key, so the id needs no separate
// management and always corresponds to the key that signs the bundles carrying it.
func installIDFromKey(pub ed25519.PublicKey) string {
	return "in_" + hex.EncodeToString(pub[:6])
}
