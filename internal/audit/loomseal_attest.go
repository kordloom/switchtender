package audit

import (
	"context"
	"encoding/json"
	"fmt"
)

// attestationAlg is the only signature algorithm a LoomSeal attestation carries.
const attestationAlg = "ed25519"

// bundleAttestation is one counter-signature in the member shape LoomSeal gives both a claim's
// and a bundle's attestations array.
type bundleAttestation struct {
	// KeyID is the sha256 fingerprint of PublicKey.
	KeyID string `json:"key_id"`
	// PublicKey is the signer's raw ed25519 public key in standard base64.
	PublicKey string `json:"public_key"`
	// Alg is the signature algorithm, always ed25519.
	Alg string `json:"alg"`
	// Role is what the signer is to the chain.
	Role string `json:"role"`
	// At is when the signer counter-signed, exactly as signed, and omitted when there is no time.
	At string `json:"at,omitempty"`
	// Sig is the base64 ed25519 signature over the domain-tagged canonical object.
	Sig string `json:"sig"`
}

// AttachAttestations places each recorded witness attestation where it rides on this bundle, drops
// the rest, and reports how many were placed. Whatever the bundle carried before is replaced.
//
// A claim attestation signs one chain link, and a verifier checks it against the link of the claim
// it sits on, so it goes on the claim whose seq and link are both the ones it names, the same match
// an anchor is held to. One that names a seq the bundle does not hold, or a link the claim at that
// seq does not carry, is dropped rather than attached: on another claim it fails verification and
// turns a good export into a failing one. A tree claim's link is its leaf hash, not the entry hash
// a witness signs, so a sparse bundle carries no claim attestation.
//
// A head attestation signs a tree root at a size, and a verifier checks it against the bundle's own
// head. It goes on only a tree bundle whose head is that size with that root and that holds every
// claim from the first through that size. A linear head's link is an entry hash, which no head
// attestation signs, and a sparse bundle discloses less than the head it carries.
//
// Attestations travel outside the producer signature, which strips them from what it covers, but
// they ride in the signed bytes only when they are on the bundle as it is signed.
func (b *Bundle) AttachAttestations(recorded []*WitnessAttestation) int {
	index := make(map[int64]int, len(b.Claims))
	for i := range b.Claims {
		b.Claims[i].Attestations = nil
		index[b.Claims[i].Chain.Seq] = i
	}
	b.Attestations = nil
	head, complete := b.completeTreeHead()
	placed := 0
	for _, a := range recorded {
		if a == nil {
			continue
		}
		switch a.Kind {
		case AttestationClaim:
			i, ok := index[a.Seq]
			if !ok || b.Claims[i].Chain.Link != a.Link {
				continue
			}
			if member, ok := a.member(); ok {
				b.Claims[i].Attestations = append(b.Claims[i].Attestations, member)
				placed++
			}
		case AttestationHead:
			if !complete || a.Seq != head.Seq || a.Size != head.Seq || a.Root != head.Link {
				continue
			}
			if member, ok := a.member(); ok {
				b.Attestations = append(b.Attestations, member)
				placed++
			}
		}
	}
	return placed
}

// AttachStoredAttestations reads the witness attestations store keeps through this bundle's head
// and places the ones that ride on it, as AttachAttestations does, reporting how many were placed.
// A store that keeps no attestations places none, which is an install never enrolled with a
// witness.
func (b *Bundle) AttachStoredAttestations(ctx context.Context, store Store) (int, error) {
	held, ok := store.(AttestationStore)
	if !ok {
		return b.AttachAttestations(nil), nil
	}
	// An attestation past the head names nothing the bundle holds, so the read stops there.
	var through int64
	if b.Chain != nil {
		through = b.Chain.Head.Seq
	}
	recorded, err := held.Attestations(ctx, through)
	if err != nil {
		return 0, fmt.Errorf("read witness attestations: %w", err)
	}
	return b.AttachAttestations(recorded), nil
}

// completeTreeHead returns this bundle's head and whether a head attestation may ride on it: the
// head is a tree root, and the claims are every position from the first through its size.
func (b *Bundle) completeTreeHead() (BundleCoord, bool) {
	if b.Chain == nil || b.Chain.Profile != TreeProfile {
		return BundleCoord{}, false
	}
	head := b.Chain.Head
	if int64(len(b.Claims)) != head.Seq {
		return head, false
	}
	for i := range b.Claims {
		if b.Claims[i].Chain.Seq != int64(i+1) {
			return head, false
		}
	}
	return head, true
}

// member encodes the attestation as the LoomSeal object a bundle carries, with every signed member
// exactly as stored. Every member is a string, so encoding cannot fail; were it ever to, false
// leaves the attestation off rather than attaching a malformed one.
func (a *WitnessAttestation) member() (json.RawMessage, bool) {
	raw, err := json.Marshal(bundleAttestation{
		KeyID: a.KeyID, PublicKey: a.PublicKey, Alg: attestationAlg, Role: a.Role, At: a.At,
		Sig: a.Sig,
	})
	if err != nil {
		return nil, false
	}
	return raw, true
}
