package handoff

import (
	"bytes"
	"crypto/ecdh"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"slices"
	"strings"
)

const (
	// PublicKeyPrefix starts the text form of a delivery public key, naming its algorithm so a later
	// key type can be told apart rather than misread.
	PublicKeyPrefix = "x25519:"
	// keyIDLabel separates a key id's hash from any other hash of the same bytes.
	keyIDLabel = "switchtender relay delivery key id v1"
	// pemType is the PEM block a private key file holds: PKCS #8, the form openssl genpkey writes.
	pemType = "PRIVATE KEY"
	// maxKeyFile bounds how much of a key file is read. A PKCS #8 X25519 key is under a hundred bytes.
	maxKeyFile = 64 << 10
	// keyFileComment opens a key file this package writes, for a reader who finds it on disk.
	keyFileComment = "# SwitchTender relay delivery key. Keep this file readable by the worker's " +
		"account only.\n"
)

// PublicKey is a relay worker pool's delivery key as the control node holds it: the key run secrets
// are sealed to.
type PublicKey struct {
	// key is the key in the form the HPKE sender takes.
	key hpke.PublicKey
	// raw is the 32-byte X25519 public key.
	raw []byte
	// id names the key without revealing anything about it beyond its identity.
	id string
}

// ParsePublicKey reads the text form of a delivery public key: PublicKeyPrefix followed by the
// standard base64 of the 32-byte X25519 public key.
func ParsePublicKey(s string) (*PublicKey, error) {
	encoded, ok := strings.CutPrefix(strings.TrimSpace(s), PublicKeyPrefix)
	if !ok {
		return nil, fmt.Errorf("%w: a delivery public key starts with %q", ErrKey, PublicKeyPrefix)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: the key is not standard base64: %w", ErrKey, err)
	}
	pub, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKey, err)
	}
	return newPublicKey(pub)
}

// newPublicKey wraps an X25519 public key for sealing.
func newPublicKey(pub *ecdh.PublicKey) (*PublicKey, error) {
	if pub.Curve() != ecdh.X25519() {
		return nil, fmt.Errorf("%w: a delivery key is an X25519 key", ErrKey)
	}
	key, err := hpke.NewDHKEMPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKey, err)
	}
	raw := pub.Bytes()
	sum := sha256.Sum256(append([]byte(keyIDLabel), raw...))
	return &PublicKey{key: key, raw: raw, id: hex.EncodeToString(sum[:16])}, nil
}

// String returns the text form ParsePublicKey reads, the line a pool file carries.
func (k *PublicKey) String() string {
	return PublicKeyPrefix + base64.StdEncoding.EncodeToString(k.raw)
}

// ID returns the key's identifier, which a sealed delivery names so the worker knows which of its
// keys opens it, and which the audit chain records beside each delivery.
func (k *PublicKey) ID() string { return k.id }

// PrivateKey is a relay worker pool's delivery key as a worker holds it: the key that opens the run
// secrets the control node sealed to the pool.
type PrivateKey struct {
	// key is the key in the form the HPKE recipient takes.
	key hpke.PrivateKey
	// ecdh is the same key in the form PKCS #8 encodes.
	ecdh *ecdh.PrivateKey
	// pub is the matching public key.
	pub *PublicKey
}

// GenerateKey returns a new delivery key from the system's random source.
func GenerateKey() (*PrivateKey, error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("%w: generate: %w", ErrKey, err)
	}
	return newPrivateKey(k)
}

// newPrivateKey wraps an X25519 private key for opening.
func newPrivateKey(k *ecdh.PrivateKey) (*PrivateKey, error) {
	if k.Curve() != ecdh.X25519() {
		return nil, fmt.Errorf("%w: a delivery key is an X25519 key", ErrKey)
	}
	key, err := hpke.NewDHKEMPrivateKey(k)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKey, err)
	}
	pub, err := newPublicKey(k.PublicKey())
	if err != nil {
		return nil, err
	}
	return &PrivateKey{key: key, ecdh: k, pub: pub}, nil
}

// Public returns the key's public half, which the pool registers with the control node.
func (k *PrivateKey) Public() *PublicKey { return k.pub }

// ID returns the identifier of the key's public half.
func (k *PrivateKey) ID() string { return k.pub.id }

// MarshalPEM encodes the key as a PKCS #8 PEM block, the form ParsePrivateKey and openssl read,
// beneath a comment saying what the file is.
func (k *PrivateKey) MarshalPEM() ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k.ecdh)
	if err != nil {
		return nil, fmt.Errorf("%w: encode: %w", ErrKey, err)
	}
	defer clear(der)
	block := pem.EncodeToMemory(&pem.Block{Type: pemType, Bytes: der})
	return append([]byte(keyFileComment), block...), nil
}

// ParsePrivateKey reads a delivery private key from one PKCS #8 PEM block, the form MarshalPEM
// writes and openssl genpkey -algorithm X25519 writes. Text before the block is allowed, so a key
// file can say what it is, and anything after it is refused, so a file holding two keys is not read
// as one.
func ParsePrivateKey(data []byte) (*PrivateKey, error) {
	block, rest := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%w: no PEM block", ErrKey)
	}
	defer clear(block.Bytes)
	if block.Type != pemType {
		return nil, fmt.Errorf("%w: the PEM block is %q, want %q", ErrKey, block.Type, pemType)
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, fmt.Errorf("%w: the file holds more than one PEM block", ErrKey)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKey, err)
	}
	k, ok := parsed.(*ecdh.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%w: the key is %T, want an X25519 key", ErrKey, parsed)
	}
	return newPrivateKey(k)
}

// LoadPrivateKeyFile reads a delivery private key from path, refusing a file another account could
// read. The key opens every secret sealed to its pool, so a copy readable by another user on the
// worker is a copy of those secrets, and the refusal comes before anything is read, the way ssh
// refuses an unprotected identity file.
func LoadPrivateKeyFile(path string) (*PrivateKey, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKeyFile, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrKeyFile, path)
	}
	if err := checkPrivate(info); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrKeyFile, path, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKeyFile, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFile+1))
	defer clear(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrKeyFile, err)
	}
	if len(data) > maxKeyFile {
		return nil, fmt.Errorf("%w: %s is larger than any delivery key", ErrKeyFile, path)
	}
	k, err := ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrKeyFile, path, err)
	}
	return k, nil
}

// checkPrivate refuses a key file whose mode lets another account read or write it. Windows keeps
// no POSIX mode bits, so its access control list is the operator's to set and nothing is checked.
func checkPrivate(info fs.FileInfo) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("mode %#o lets another account read it; restrict it with chmod 600", perm)
	}
	return nil
}

// WriteKeyFile writes a new key to path, mode 0600, and refuses to replace a file already there: a
// key overwritten by mistake is every sealed delivery to its pool failing until the pool registers
// another.
func WriteKeyFile(path string, k *PrivateKey) error {
	data, err := k.MarshalPEM()
	if err != nil {
		return err
	}
	defer clear(data)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrKeyFile, err)
	}
	_, werr := f.Write(data)
	if err := errors.Join(werr, f.Close()); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("%w: write %s: %w", ErrKeyFile, path, err)
	}
	return nil
}

// KeyRing is the set of delivery keys one worker holds. It holds more than one while its pool
// rotates: the old key opens what was sealed before the control node switched, and the new key
// opens everything after.
type KeyRing struct {
	// keys maps a key id to the key.
	keys map[string]*PrivateKey
}

// NewKeyRing returns a ring holding keys. The same key given twice is refused, since it is a
// configuration mistake that would otherwise read as a rotation.
func NewKeyRing(keys ...*PrivateKey) (*KeyRing, error) {
	r := &KeyRing{keys: make(map[string]*PrivateKey, len(keys))}
	for _, k := range keys {
		if k == nil {
			return nil, fmt.Errorf("%w: nil key", ErrKey)
		}
		if _, dup := r.keys[k.ID()]; dup {
			return nil, fmt.Errorf("%w: key %s is given twice", ErrKey, k.ID())
		}
		r.keys[k.ID()] = k
	}
	return r, nil
}

// IDs returns the ids of the keys the ring holds, sorted.
func (r *KeyRing) IDs() []string {
	if r == nil {
		return nil
	}
	ids := make([]string, 0, len(r.keys))
	for id := range r.keys {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// Len reports how many keys the ring holds.
func (r *KeyRing) Len() int {
	if r == nil {
		return 0
	}
	return len(r.keys)
}

// key returns the key with the given id, or nil.
func (r *KeyRing) key(id string) *PrivateKey {
	if r == nil {
		return nil
	}
	return r.keys[id]
}
