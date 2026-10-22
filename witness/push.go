package witness

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kordloom/loomseal/jcs"
	"github.com/kordloom/loomseal/seal"

	"github.com/kordloom/switchtender/identity"
)

// The protocol objects an enrolled install and a push witness exchange each carry a domain member,
// named by domainMember, whose value names the object and its version. The member sits inside the
// signed bytes, so a signature made over one kind of object never verifies as another kind whose
// remaining members happen to line up.
const (
	// domainMember is the member that carries an object's domain tag.
	domainMember = "switchtender"
	// sigMember is the member a signature travels in, left out of the bytes it covers.
	sigMember = "sig"
	// PushVersion tags a push, the producer-signed report of one appended beat.
	PushVersion = "witness-push/1"
	// EnrollVersion tags an enrollment request, signed by the producer key it enrolls.
	EnrollVersion = "witness-enroll/1"
	// EnrolledVersion tags the witness's signed answer to an enrollment.
	EnrolledVersion = "witness-enrolled/1"
	// KeysVersion tags the witness's signed list of its own keys.
	KeysVersion = "witness-keys/1"
)

// Push is one appended beat as an enrolled install reports it to a push witness, with the tree root
// at the same size and a consistency proof from the size the witness last acknowledged. It carries
// coordinates and hashes only: no run content, host name, or address.
type Push struct {
	// Switchtender is the domain tag, PushVersion.
	Switchtender string `json:"switchtender"`
	// InstallID names the pushing install, the id derived from its producer key.
	InstallID string `json:"install_id"`
	// BeatNumber is the span beat's number, rising by exactly one per beat.
	BeatNumber int64 `json:"beat"`
	// At is when the beat was appended, as the beat feed serves it.
	At string `json:"at"`
	// Seq is the beat entry's position in the audit chain.
	Seq int64 `json:"seq"`
	// Head is the hex chain link of the entry at Seq.
	Head string `json:"head"`
	// Size is the number of chain entries the tree root covers.
	Size int64 `json:"size"`
	// Root is the hex Merkle tree root over the first Size entries.
	Root string `json:"root"`
	// FromSize is the tree size the witness last acknowledged, the base the proof starts from.
	FromSize int64 `json:"from_size"`
	// Proof is the hex consistency proof from the tree at FromSize to the tree at Size.
	Proof []string `json:"proof"`
	// Sig is the base64 producer signature over the canonical push without this member.
	Sig string `json:"sig,omitempty"`
}

// Beat returns the push's beat in the feed's shape, so a push is checked by the same rules a pulled
// feed answer is.
func (p *Push) Beat() Beat {
	return Beat{Beat: p.BeatNumber, At: p.At, Seq: p.Seq, Head: p.Head}
}

// EnrollRequest asks a push witness to remember an install. It is signed by the producer key it
// names, which proves the requester holds that key, and it carries the signed license file the
// witness's enrollment policy reads. It carries no URL, organization, or contact.
type EnrollRequest struct {
	// Switchtender is the domain tag, EnrollVersion.
	Switchtender string `json:"switchtender"`
	// InstallID names the install to enroll. It must be an id PublicKey was born to, see BornTo.
	InstallID string `json:"install_id"`
	// PublicKey is the producer's raw ed25519 public key in standard base64, the key every later
	// push from this install is signed by.
	PublicKey string `json:"public_key"`
	// License is the signed license file, carried verbatim as JSON for the enrollment policy.
	License json.RawMessage `json:"license,omitempty"`
	// At is when the request was made, RFC 3339 in UTC.
	At string `json:"at"`
	// Sig is the base64 producer signature over the canonical request without this member.
	Sig string `json:"sig,omitempty"`
}

// WitnessKey is one signing key a witness has used, with the period it speaks for.
type WitnessKey struct {
	// KeyID is the key's sha256 fingerprint, the value a relying party pins.
	KeyID string `json:"key_id"`
	// PublicKey is the raw ed25519 public key in standard base64.
	PublicKey string `json:"public_key"`
	// ValidFrom is when the key began signing, RFC 3339 in UTC.
	ValidFrom string `json:"valid_from"`
	// Retired is when the key stopped being the witness's word, empty while it is in use.
	Retired string `json:"retired,omitempty"`
}

// WitnessKeys is the witness's own key list: the current key and every key it has signed with. A
// client accepts a new list only when a key it already holds signed it, so a rotation is
// authenticated by the chain of keys.
type WitnessKeys struct {
	// Switchtender is the domain tag, KeysVersion.
	Switchtender string `json:"switchtender"`
	// KeyID is the current key's sha256 fingerprint.
	KeyID string `json:"key_id"`
	// PublicKey is the current key's raw ed25519 public key in standard base64.
	PublicKey string `json:"public_key"`
	// Keys lists every key the witness has signed with, the current one included.
	Keys []WitnessKey `json:"keys"`
	// Sig is the base64 signature by the current key over the canonical list without this member.
	Sig string `json:"sig,omitempty"`
	// Endorsements are signatures by every retired key the witness still holds, over the same bytes
	// Sig covers: the canonical list without its sig and endorsements members.
	Endorsements []KeySignature `json:"endorsements,omitempty"`
}

// EnrollResponse is the witness's signed answer to an enrollment. Enrolling again answers the same.
type EnrollResponse struct {
	// Switchtender is the domain tag, EnrolledVersion.
	Switchtender string `json:"switchtender"`
	// Witness names the witness's keys, so the install can pin the one that countersigns it.
	Witness WitnessKeys `json:"witness"`
	// InstallID is the enrolled install's label, the id its producer key was born to.
	InstallID string `json:"install_id"`
	// KeyID is the enrolled producer key's sha256 fingerprint, the key the witness keeps the
	// install's memory under, echoed so the install confirms what was bound.
	KeyID string `json:"key_id"`
	// PublicKey is the enrolled producer's raw ed25519 public key in standard base64.
	PublicKey string `json:"public_key"`
	// EnrolledAt is when the witness first enrolled the install, RFC 3339 in UTC.
	EnrolledAt string `json:"enrolled_at"`
	// MaxInstalls is how many installs one license may enroll.
	MaxInstalls int `json:"max_installs"`
	// PushPath is the path the install posts its beats to.
	PushPath string `json:"push_path"`
	// Sig is the base64 witness signature over the canonical answer without this member.
	Sig string `json:"sig,omitempty"`
}

// SignPush stamps the push with its domain tag and signs it with the producer identity.
func SignPush(p *Push, id identity.Identity) error {
	if p == nil {
		return fmt.Errorf("%w: sign push: no push", ErrMalformed)
	}
	p.Switchtender = PushVersion
	sig, err := signTagged(PushVersion, p, id.Private())
	if err != nil {
		return fmt.Errorf("sign push: %w", err)
	}
	p.Sig = sig
	return nil
}

// VerifyPush checks that the push carries its domain tag and that pub signed every other member of
// it. pub is the producer key the install enrolled with.
func VerifyPush(p *Push, pub ed25519.PublicKey) error {
	if p == nil {
		return fmt.Errorf("%w: verify push: no push", ErrMalformed)
	}
	if p.Switchtender != PushVersion {
		return fmt.Errorf("%w: push is tagged %q, not %q", ErrMalformed, clip(p.Switchtender, 32),
			PushVersion)
	}
	if err := verifyTagged(PushVersion, p, pub, p.Sig); err != nil {
		return fmt.Errorf("verify push: %w", err)
	}
	return nil
}

// SignEnroll stamps the request with its domain tag and the identity's public key, and signs it
// with that identity, so the request proves possession of the key it enrolls.
func SignEnroll(r *EnrollRequest, id identity.Identity) error {
	if r == nil {
		return fmt.Errorf("%w: sign enrollment: no request", ErrMalformed)
	}
	r.Switchtender = EnrollVersion
	r.PublicKey = id.PublicKeyBase64()
	sig, err := signTagged(EnrollVersion, r, id.Private())
	if err != nil {
		return fmt.Errorf("sign enrollment: %w", err)
	}
	r.Sig = sig
	return nil
}

// VerifyEnroll checks that the request carries its domain tag and an install id, that the key it
// names signed every other member of it, and that the key was born to that install id. Whether the
// license lets the key enroll at all is the enrollment policy's question, not this one's.
func VerifyEnroll(r *EnrollRequest) error {
	if r == nil {
		return fmt.Errorf("%w: verify enrollment: no request", ErrMalformed)
	}
	if r.Switchtender != EnrollVersion {
		return fmt.Errorf("%w: enrollment is tagged %q, not %q", ErrMalformed,
			clip(r.Switchtender, 32), EnrollVersion)
	}
	if r.InstallID == "" {
		return fmt.Errorf("%w: enrollment names no install", ErrMalformed)
	}
	pub, err := decodeKey(r.PublicKey)
	if err != nil {
		return fmt.Errorf("verify enrollment: %w", err)
	}
	if err := verifyTagged(EnrollVersion, r, pub, r.Sig); err != nil {
		return fmt.Errorf("verify enrollment: %w", err)
	}
	if !BornTo(r.InstallID, pub) {
		return fmt.Errorf("%w: %s was not born to key %s", ErrNotBorn, clip(r.InstallID, 64),
			seal.KeyID(pub))
	}
	return nil
}

// BornTo reports whether installID is an id pub was born to: the wide form every install mints, or
// the narrow form an install born before the widening keeps. These are the two derivations the
// bundle verifier accepts for a producer without an explicit acceptance. A rotated identity, which
// keeps its old id over a new key, matches neither form and is refused.
func BornTo(installID string, pub ed25519.PublicKey) bool {
	if installID == "" || len(pub) != ed25519.PublicKeySize {
		return false
	}
	return installID == identity.InstallIDFromKey(pub) ||
		installID == identity.LegacyInstallIDFromKey(pub)
}

// taggedContent returns the RFC 8785 canonical form of v without its sig member and with its domain
// member set to tag. Every member v serializes is covered, so a member added to a protocol object
// is signed without anyone remembering to list it.
func taggedContent(tag string, v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%w: encode: %w", ErrMalformed, err)
	}
	parsed, err := jcs.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: canonicalize: %w", ErrMalformed, err)
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: not a JSON object", ErrMalformed)
	}
	delete(obj, sigMember)
	obj[domainMember] = tag
	content, err := jcs.Serialize(obj)
	if err != nil {
		return nil, fmt.Errorf("%w: canonicalize: %w", ErrMalformed, err)
	}
	return content, nil
}

// signTagged signs v's tagged canonical content with priv and returns the base64 signature.
func signTagged(tag string, v any, priv ed25519.PrivateKey) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("%w: no signing key", ErrMalformed)
	}
	content, err := taggedContent(tag, v)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, content)), nil
}

// verifyTagged checks a base64 signature by pub over v's tagged canonical content.
func verifyTagged(tag string, v any, pub ed25519.PublicKey, sig string) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: the verifying key is not a 32 byte ed25519 key", ErrMalformed)
	}
	if sig == "" {
		return fmt.Errorf("%w: unsigned", ErrSignature)
	}
	raw, err := decodeBase64(sig)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return fmt.Errorf("%w: the signature is not a base64 ed25519 signature", ErrMalformed)
	}
	content, err := taggedContent(tag, v)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, content, raw) {
		return fmt.Errorf("%w: does not verify under key %s", ErrSignature, seal.KeyID(pub))
	}
	return nil
}

// decodeBase64 decodes standard base64, refusing line breaks, which the standard decoder skips and
// which would give one value two encodings.
func decodeBase64(s string) ([]byte, error) {
	if strings.ContainsAny(s, "\r\n") {
		return nil, fmt.Errorf("%w: base64 contains a line break", ErrMalformed)
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: base64: %w", ErrMalformed, err)
	}
	return raw, nil
}

// decodeKey decodes a base64 raw ed25519 public key.
func decodeKey(s string) (ed25519.PublicKey, error) {
	raw, err := decodeBase64(s)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: public key is not a base64 32 byte ed25519 key", ErrMalformed)
	}
	return ed25519.PublicKey(raw), nil
}
