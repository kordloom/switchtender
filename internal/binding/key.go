// Package binding seals the rows the executor acts on under a key the database never holds, and
// verifies those seals before anything executes. A seal is an HMAC over a deterministic record of
// the row's spec binding, the fields that govern its approval, its parent's binding, and the chain
// entries it derives from, so a principal who can write every table but does not hold the key
// cannot produce a seal for a row it edited.
package binding

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

const (
	// keyInfo is the HKDF info string that separates the binding key from every other key the
	// producer identity's seed could be stretched into.
	keyInfo = "switchtender approval binding v1"
	// keySize is the length of the derived HMAC-SHA256 key in bytes.
	keySize = 32
	// minSeedSize is the shortest seed DeriveKey accepts, the length of an Ed25519 seed.
	minSeedSize = 32
	// adoptionMarker is the string the id of an install's adoption record is the keyed digest of.
	adoptionMarker = "switchtender adoption marker v1"
	// adoptionIDHexLen is how many hex digits of that digest the id keeps after its prefix, the
	// width every chain entry id fixed by a digest keeps.
	adoptionIDHexLen = 32
)

// Key is the binding key a process seals and verifies rows with. It is derived once at start and
// held only in memory: it has no exported field, refuses to marshal, and prints as a fixed
// placeholder under every fmt verb.
type Key struct {
	// mac is the HMAC-SHA256 key.
	mac [keySize]byte
}

// DeriveKey derives the binding key from the producer identity's seed with HKDF-SHA256, an empty
// salt, and the info string "switchtender approval binding v1". The derivation keeps the signing
// seed and the MAC key independent, so neither use weakens the other.
func DeriveKey(seed []byte) (*Key, error) {
	if len(seed) < minSeedSize {
		return nil, fmt.Errorf("%w: got %d", ErrShortSeed, len(seed))
	}
	raw, err := hkdf.Key(sha256.New, seed, nil, keyInfo, keySize)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDerive, err)
	}
	k := &Key{}
	copy(k.mac[:], raw)
	clear(raw)
	return k, nil
}

// AdoptionID returns the id an install's adoption record is appended under: "aud_" and the first
// 32 hex digits of the HMAC-SHA256 under k over "switchtender adoption marker v1". Every replica
// holding the key computes the same id, so a second adoption collides with the first on the chain's
// primary key, and nobody without the key can compute it, so an entry planted before adoption
// cannot stand in for the record.
func (k *Key) AdoptionID() string {
	m := hmac.New(sha256.New, k.mac[:])
	m.Write([]byte(adoptionMarker))
	return "aud_" + hex.EncodeToString(m.Sum(nil))[:adoptionIDHexLen]
}

// MarshalJSON refuses, so a key that ends up inside a value something serializes stops that write
// instead of riding along in it.
func (k Key) MarshalJSON() ([]byte, error) {
	return nil, ErrSerialize
}

// Format prints a fixed placeholder for every verb, so a key that reaches a log line or an error
// message never carries its bytes with it.
func (k Key) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, "binding.Key(redacted)")
}
