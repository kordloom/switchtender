package credential

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters for deriving the AES key from the operator passphrase. The derivation runs
// once at startup, so a single deployment pays the cost one time while an offline guess against a
// leaked database stays expensive. Memory is in KiB.
const (
	// argonTime is the number of argon2id passes.
	argonTime = 3
	// argonMemory is the argon2id memory cost in KiB, here 64 MiB.
	argonMemory = 64 * 1024
	// argonThreads is the argon2id parallelism.
	argonThreads = 4
	// argonKeyLen is the derived key length in bytes, matching AES-256.
	argonKeyLen = 32
)

// Sealer encrypts and decrypts credential secrets with AES-256-GCM. The key derives from an
// operator supplied passphrase through argon2id salted with a per-deployment value, so a leaked
// database plus a weak passphrase is expensive to brute force and a key is never shared across
// deployments.
type Sealer struct {
	// key is the derived 32 byte AES key.
	key [32]byte
	// ok reports whether a key was configured.
	ok bool
}

// NewSealer derives a Sealer from a passphrase and a per-deployment salt using argon2id. Both must
// be non-empty; an empty passphrase or salt yields a disabled Sealer whose operations return
// ErrNoKey, so credential features fail loudly instead of storing plaintext. The salt must stay
// stable across restarts or existing ciphertext cannot be decrypted.
func NewSealer(passphrase, salt string) *Sealer {
	if passphrase == "" || salt == "" {
		return &Sealer{}
	}
	// Normalize the operator salt to a fixed width so any salt string is a valid argon2id salt; its
	// uniqueness, not its length or encoding, is what matters.
	saltSum := sha256.Sum256([]byte(salt))
	derived := argon2.IDKey([]byte(passphrase), saltSum[:], argonTime, argonMemory, argonThreads, argonKeyLen)
	s := &Sealer{ok: true}
	copy(s.key[:], derived)
	// Wipe the transient derived slice; the fixed array holds the working key.
	for i := range derived {
		derived[i] = 0
	}
	return s
}

// Enabled reports whether the Sealer has a key.
func (s *Sealer) Enabled() bool {
	return s.ok
}

// Seal encrypts plaintext and returns base64 of nonce plus ciphertext.
func (s *Sealer) Seal(plaintext string) (string, error) {
	if !s.ok {
		return "", ErrNoKey
	}
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return "", fmt.Errorf("seal: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("seal: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("seal: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Open decrypts a sealed secret produced by Seal.
func (s *Sealer) Open(sealed string) (string, error) {
	if !s.ok {
		return "", ErrNoKey
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	if len(raw) < gcm.NonceSize() {
		return "", fmt.Errorf("open: sealed value too short")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	return string(plain), nil
}
