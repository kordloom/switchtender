package template

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"strings"
)

// hostConfigKeyPrefix marks a minted callback key so a leaked string is recognizable.
const hostConfigKeyPrefix = "hck_"

// NewHostConfigKey mints a provisioning callback key. The plaintext is shown once, sealed at rest
// with the server key, and baked into whatever a host runs at boot to call back.
func NewHostConfigKey() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hostConfigKeyPrefix + hex.EncodeToString(b[:]), nil
}

// HostConfigKeyMatches reports whether presented is the template's key, comparing in constant time
// so the time a refusal takes says nothing about how much of a guess was right. An empty key never
// matches, so a template with no key minted refuses every callback.
func HostConfigKeyMatches(key, presented string) bool {
	presented = strings.TrimSpace(presented)
	if key == "" || presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(key), []byte(presented)) == 1
}
