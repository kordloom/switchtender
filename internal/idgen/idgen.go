// Package idgen mints the random object identifiers every entity package uses, one implementation
// instead of a copy per package.
package idgen

import (
	"crypto/rand"
	"encoding/hex"
)

// New returns a random identifier of prefix followed by n random bytes hex encoded. It panics when
// the system random source fails, a developer-environment error no caller can act on.
func New(prefix string, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("idgen: read random: " + err.Error())
	}
	return prefix + hex.EncodeToString(b)
}
