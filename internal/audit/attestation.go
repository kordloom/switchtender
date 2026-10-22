package audit

import (
	"context"
	"fmt"

	"github.com/kordloom/switchtender/internal/idgen"
)

// Witness attestation kinds, the two LoomSeal countersignatures a push witness answers a beat with.
const (
	// AttestationClaim is a LoomSeal attestation/1 over one claim's chain link. Seq is that entry's
	// sequence and Link its hash, and it rides on that claim in a bundle.
	AttestationClaim = "claim"
	// AttestationHead is a LoomSeal head-attestation/1 over a tree root at a size. Seq and Size are
	// both the tree size and Root the root, and it rides at the top of a bundle whose head is that
	// root and size.
	AttestationHead = "head"
)

// WitnessAttestation is one countersignature a hosted witness returned for a pushed beat, kept so
// the bundles and receipts drawn from the chain later carry it.
//
// KeyID, PublicKey, Role, At, and Sig are the LoomSeal attestation members exactly as the witness
// signed them. They are stored verbatim, At included, because the signature covers those bytes and
// a value rewritten on the way through the database would no longer verify.
type WitnessAttestation struct {
	// ID is the unique attestation identifier.
	ID string `json:"id"`
	// Seq is the chain coordinate the attestation covers: the entry's sequence for a claim
	// attestation, the tree size for a head attestation.
	Seq int64 `json:"seq"`
	// Link is the entry hash at Seq that a claim attestation signs, empty on a head attestation.
	Link string `json:"link,omitempty"`
	// Kind is claim or head.
	Kind string `json:"kind"`
	// Size is the tree size a head attestation signs, zero on a claim attestation.
	Size int64 `json:"size,omitempty"`
	// Root is the Merkle root over the first Size entries a head attestation signs, empty on a
	// claim attestation.
	Root string `json:"root,omitempty"`
	// KeyID is the sha256 fingerprint of PublicKey, the key of the witness that signed.
	KeyID string `json:"key_id"`
	// PublicKey is the signing witness's raw ed25519 public key in standard base64.
	PublicKey string `json:"public_key"`
	// Role is what the signer is to the chain, witness for a push witness.
	Role string `json:"role"`
	// At is when the witness signed, in the LoomSeal time form exactly as it was signed.
	At string `json:"at"`
	// Sig is the base64 ed25519 signature over the domain-tagged canonical object.
	Sig string `json:"sig"`
	// Witness is the key id of the witness this install is enrolled with, the key it pinned at
	// enrollment. It equals KeyID unless the witness signed with a key other than the pinned one,
	// which is what a witness key rotation produces.
	Witness string `json:"witness"`
}

// AttestationStore persists witness attestations. Implementations must be safe for concurrent use.
type AttestationStore interface {
	// SaveAttestation records one attestation. The store holds one record per seq, kind, and key
	// id, and the first one written wins and never changes. Saving a record identical to the held
	// one in every field but ID succeeds without writing and sets the caller's ID to the held
	// record's. Saving a different record at the same coordinates writes nothing and returns
	// ErrAttestationConflict.
	SaveAttestation(ctx context.Context, a *WitnessAttestation) error
	// Attestations returns every attestation at or below seq, oldest first with ties broken on id,
	// so a bundle carries the attestations that cover the range it holds. A seq of zero or less
	// returns all of them.
	Attestations(ctx context.Context, seq int64) ([]*WitnessAttestation, error)
	// DeleteAttestation removes the attestation with the given id, so one recorded in error can be
	// withdrawn. It returns ErrAttestationNotFound when no attestation carries that id.
	DeleteAttestation(ctx context.Context, id string) error
}

// NewAttestationID returns a random witness attestation identifier prefixed with "att_".
func NewAttestationID() string { return idgen.New("att_", 6) }

// ValidAttestationKind reports whether k is a witness attestation kind: claim or head.
func ValidAttestationKind(k string) bool {
	return k == AttestationClaim || k == AttestationHead
}

// SameAs reports whether a and b record the same countersignature, every field but ID equal.
func (a *WitnessAttestation) SameAs(b *WitnessAttestation) bool {
	x, y := *a, *b
	x.ID, y.ID = "", ""
	return x == y
}

// ResolveHeldAttestation settles a save that found held already recorded at a's seq, kind, and key
// id. An identical record is the same answer saved again, so it succeeds and a takes held's ID.
// Any other record is refused with ErrAttestationConflict and a is left as it was. held is never
// changed either way.
func ResolveHeldAttestation(held, a *WitnessAttestation) error {
	if !held.SameAs(a) {
		return fmt.Errorf("save attestation at seq %d kind %s key %s: held as %s: %w", a.Seq,
			a.Kind, a.KeyID, held.ID, ErrAttestationConflict)
	}
	a.ID = held.ID
	return nil
}
