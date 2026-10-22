package witness

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"regexp"
	"time"

	"github.com/kordloom/loomseal/jcs"
	"github.com/kordloom/loomseal/seal"

	"github.com/kordloom/switchtender/identity"
)

// A push witness countersigns in the LoomSeal format's own construction rather than a shape of its
// own: a claim attestation over the link at the pushed position and a head attestation over the
// tree root at the pushed size, each with role witness and its signing time. Any LoomSeal verifier
// checks them with no knowledge of this product.
const (
	// RoleWitness is the role a push witness signs under.
	RoleWitness = "witness"
	// sealAlg is the only signature algorithm a LoomSeal attestation carries.
	sealAlg = "ed25519"
	// claimAttestationTag is the LoomSeal domain tag of a claim attestation.
	claimAttestationTag = "attestation/1"
	// headAttestationTag is the LoomSeal domain tag of a head attestation.
	headAttestationTag = "head-attestation/1"
)

// hashHex matches a sha256 digest in lowercase hex, the form of a chain link and a tree root.
var hashHex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// sealTime matches the one time form LoomSeal accepts: seconds, an optional fraction, and Z.
var sealTime = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}` +
	`(\.[0-9]+)?Z$`)

// Countersignature is one LoomSeal attestation a witness mints, in the exact member shape the
// format gives a claim's or a bundle's attestations array.
type Countersignature struct {
	// KeyID is the sha256 fingerprint of PublicKey.
	KeyID string `json:"key_id"`
	// PublicKey is the signer's raw ed25519 public key in standard base64.
	PublicKey string `json:"public_key"`
	// Alg is the signature algorithm, always ed25519.
	Alg string `json:"alg"`
	// Role is what the signer is to the chain, witness for a push witness.
	Role string `json:"role"`
	// At is when the signer countersigned, in the LoomSeal time form, and it is signed.
	At string `json:"at,omitempty"`
	// Sig is the base64 ed25519 signature over the domain-tagged canonical object.
	Sig string `json:"sig"`
}

// ClaimAttestation countersigns the claim whose chain link is link, as a witness, at time at. The
// result rides on that claim in a LoomSeal bundle.
func ClaimAttestation(link string, at time.Time, id identity.Identity) (Countersignature, error) {
	if !hashHex.MatchString(link) {
		return Countersignature{}, fmt.Errorf("%w: claim attestation: link is not 64 hex",
			ErrMalformed)
	}
	when, err := formatSealTime(at)
	if err != nil {
		return Countersignature{}, fmt.Errorf("claim attestation: %w", err)
	}
	return mint(claimPreimage(link, RoleWitness, when), RoleWitness, when, id.Private())
}

// HeadAttestation countersigns the tree head whose root is root over size entries, as a witness, at
// time at. The result rides at the top level of a LoomSeal bundle whose head is that root and size.
func HeadAttestation(root string, size int64, at time.Time, id identity.Identity) (Countersignature, error) {
	if !hashHex.MatchString(root) {
		return Countersignature{}, fmt.Errorf("%w: head attestation: root is not 64 hex",
			ErrMalformed)
	}
	if size < 1 {
		return Countersignature{}, fmt.Errorf("%w: head attestation: size %d is not positive",
			ErrMalformed, size)
	}
	when, err := formatSealTime(at)
	if err != nil {
		return Countersignature{}, fmt.Errorf("head attestation: %w", err)
	}
	return mint(headPreimage(root, size, RoleWitness, when), RoleWitness, when, id.Private())
}

// VerifyClaimAttestation checks a witness's countersignature of the claim whose link is link: the
// key id is the key's digest, the role is witness, the time is present and in form, and the
// signature holds over the LoomSeal claim attestation object. Which key to trust is the caller's to
// decide by KeyID.
func VerifyClaimAttestation(link string, c Countersignature) error {
	if err := checkCountersignature(c); err != nil {
		return fmt.Errorf("claim attestation: %w", err)
	}
	if err := verifyMinted(claimPreimage(link, c.Role, c.At), c); err != nil {
		return fmt.Errorf("claim attestation: %w", err)
	}
	return nil
}

// VerifyHeadAttestation checks a witness's countersignature of the tree head whose root is root
// over size entries, by the same rules as VerifyClaimAttestation over the LoomSeal head attestation
// object.
func VerifyHeadAttestation(root string, size int64, c Countersignature) error {
	if err := checkCountersignature(c); err != nil {
		return fmt.Errorf("head attestation: %w", err)
	}
	if err := verifyMinted(headPreimage(root, size, c.Role, c.At), c); err != nil {
		return fmt.Errorf("head attestation: %w", err)
	}
	return nil
}

// claimPreimage is the object a LoomSeal claim attestation signs, with at present exactly when the
// attestation carries one.
func claimPreimage(link, role, at string) map[string]any {
	obj := map[string]any{"loomseal": claimAttestationTag, "link": link, "role": role}
	if at != "" {
		obj["at"] = at
	}
	return obj
}

// headPreimage is the object a LoomSeal head attestation signs: the head's link, which in the tree
// profile is the root, and its seq, which is the tree size.
func headPreimage(root string, size int64, role, at string) map[string]any {
	obj := map[string]any{"loomseal": headAttestationTag, "link": root, "seq": size, "role": role}
	if at != "" {
		obj["at"] = at
	}
	return obj
}

// mint signs a preimage object with priv and returns the attestation that carries it.
func mint(obj map[string]any, role, at string, priv ed25519.PrivateKey) (Countersignature, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return Countersignature{}, fmt.Errorf("%w: no signing key", ErrMalformed)
	}
	preimage, err := jcs.Serialize(obj)
	if err != nil {
		return Countersignature{}, fmt.Errorf("%w: canonicalize: %w", ErrMalformed, err)
	}
	pub, _ := priv.Public().(ed25519.PublicKey)
	return Countersignature{
		KeyID:     seal.KeyID(pub),
		PublicKey: base64.StdEncoding.EncodeToString(pub),
		Alg:       sealAlg,
		Role:      role,
		At:        at,
		Sig:       base64.StdEncoding.EncodeToString(ed25519.Sign(priv, preimage)),
	}, nil
}

// checkCountersignature checks the members of a witness attestation that do not depend on what it
// signs.
func checkCountersignature(c Countersignature) error {
	if c.Alg != sealAlg {
		return fmt.Errorf("%w: alg %q, want %s", ErrMalformed, clip(c.Alg, 32), sealAlg)
	}
	if c.Role != RoleWitness {
		return fmt.Errorf("%w: role %q, want %s", ErrMalformed, clip(c.Role, 32), RoleWitness)
	}
	if !validSealTime(c.At) {
		return fmt.Errorf("%w: at %q is not a LoomSeal time", ErrMalformed, clip(c.At, 40))
	}
	return nil
}

// verifyMinted checks that the attestation's key is the one its key id names and that it signed
// obj.
func verifyMinted(obj map[string]any, c Countersignature) error {
	pub, err := decodeKey(c.PublicKey)
	if err != nil {
		return err
	}
	if seal.KeyID(pub) != c.KeyID {
		return fmt.Errorf("%w: key_id does not match its public key", ErrMalformed)
	}
	sig, err := decodeBase64(c.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("%w: sig is not a base64 ed25519 signature", ErrMalformed)
	}
	preimage, err := jcs.Serialize(obj)
	if err != nil {
		return fmt.Errorf("%w: canonicalize: %w", ErrMalformed, err)
	}
	if !ed25519.Verify(pub, preimage, sig) {
		return fmt.Errorf("%w: does not verify under key %s", ErrSignature, c.KeyID)
	}
	return nil
}

// formatSealTime writes t in the LoomSeal time form: UTC, trailing fractional zeros trimmed, and
// the fraction omitted when it is zero. A zero time or a year outside 0001 to 9999 is refused,
// since the form has no way to write it.
func formatSealTime(t time.Time) (string, error) {
	if t.IsZero() {
		return "", fmt.Errorf("%w: no signing time", ErrMalformed)
	}
	t = t.UTC()
	if y := t.Year(); y < 1 || y > 9999 {
		return "", fmt.Errorf("%w: year %d is outside the LoomSeal time form", ErrMalformed, y)
	}
	return t.Format(time.RFC3339Nano), nil
}

// validSealTime reports whether s is a time in the LoomSeal form and names a real instant, so a
// day past the end of its month or a leap second is refused.
func validSealTime(s string) bool {
	if !sealTime.MatchString(s) {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	return err == nil && t.Year() >= 1
}
