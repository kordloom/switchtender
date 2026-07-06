package credential

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// Sealer encrypts and decrypts credential secrets with AES-256-GCM. The key derives from an
// operator supplied passphrase, so the database alone never suffices to read secrets.
type Sealer struct {
	// key is the derived 32 byte AES key.
	key [32]byte
	// ok reports whether a key was configured.
	ok bool
}

// NewSealer derives a Sealer from a passphrase. An empty passphrase yields a disabled Sealer
// whose operations return ErrNoKey, so credential features fail loudly instead of storing
// plaintext.
func NewSealer(passphrase string) *Sealer {
	if passphrase == "" {
		return &Sealer{}
	}
	return &Sealer{key: sha256.Sum256([]byte(passphrase)), ok: true}
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
