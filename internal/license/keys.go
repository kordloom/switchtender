package license

import (
	"crypto/ed25519"
	"encoding/hex"
)

// publicKeys are the signing keys this build trusts, by kid. Adding a key is a release; removing
// one revokes every license it signed, which is the whole revocation mechanism: no list is fetched,
// because nothing is ever fetched.
// IssuerKid is the kid the mint command stamps into every license it signs. It must name a key in
// publicKeys or minting refuses at the source.
const IssuerKid = "k1"

var publicKeys = map[string]ed25519.PublicKey{
	// k1 is the launch issuer key. The private seed lives offline with the founder; it has never
	// been in this repository or any release artifact.
	"k1": mustKey("5385864df8567218ed8d900c04fc58f5a34a40ca653669b0b79529d4743e1f2d"),
}

// mustKey decodes a compiled-in public key. A typo here is a build worth failing.
func mustKey(hexKey string) ed25519.PublicKey {
	raw, err := hex.DecodeString(hexKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		panic("license: bad compiled-in key: " + hexKey)
	}
	return ed25519.PublicKey(raw)
}

// RegisterKey adds a trusted signing key. The real key ships compiled in; tests register their own.
func RegisterKey(kid string, pub ed25519.PublicKey) { publicKeys[kid] = pub }
