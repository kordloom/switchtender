package audit

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kordloom/loomseal/seal"
)

// LoomSeal format constants. The bundle declares the format version it conforms to, the profile that
// says how its chain links are constructed, and the claim type that names what its payloads mean.
const (
	// loomsealVersion is the format version this emitter writes.
	loomsealVersion = "0.1"
	// ChainProfile names the chain construction an audit bundle uses. Its links are the same hashes
	// the audit chain already stores, so any verifier recomputes them from the claims alone.
	ChainProfile = "switchtender-audit-v1"
	// ClaimType names the payload shape of an audit claim.
	ClaimType = "switchtender.audit/1"
	// ProductName identifies the producer in every bundle.
	ProductName = "switchtender"
)

// Bundle is a LoomSeal bundle: a signed, self-describing record a third party verifies offline with
// an open verifier, without trusting or installing SwitchTender.
//
// The field order here is only for reading. A bundle is signed over its RFC 8785 canonical form, in
// which members are ordered by their keys, so the order this struct marshals in does not matter.
type Bundle struct {
	// LoomSeal is the format version.
	LoomSeal string `json:"loomseal"`
	// BundleID identifies this bundle.
	BundleID string `json:"bundle_id"`
	// CreatedAt is when the bundle was assembled.
	CreatedAt string `json:"created_at"`
	// Producer says who emitted the bundle and which key signed it.
	Producer BundleProducer `json:"producer"`
	// Subject says what the claims are about.
	Subject BundleSubject `json:"subject"`
	// Chain carries the profile and the head the producer attests for the whole chain.
	Chain *BundleChain `json:"chain,omitempty"`
	// Claims are the audit entries, ascending by sequence and contiguous.
	Claims []BundleClaim `json:"claims"`
	// Signatures holds the producer signature. It is emptied and recomputed when the bundle is
	// signed, so it is written as an empty array here.
	Signatures []any `json:"signatures"`
}

// BundleProducer identifies the install and the key a relying party pins.
type BundleProducer struct {
	// Product is the emitting product's name.
	Product string `json:"product"`
	// ProductVersion is the emitting build's version.
	ProductVersion string `json:"product_version"`
	// InstallID distinguishes this install from another running the same product.
	InstallID string `json:"install_id"`
	// PublicKey is the raw ed25519 public key in standard base64. The existing signed export writes
	// the same key in hex; this is the same key and the same trust in one envelope.
	PublicKey string `json:"public_key"`
	// KeyID is the sha256 fingerprint of the raw key bytes, the value published on a trust page.
	KeyID string `json:"key_id"`
}

// BundleSubject says what a bundle's claims are about. An audit chain is about the install itself.
type BundleSubject struct {
	// Type is the kind of thing the claims concern.
	Type string `json:"type"`
	// ID identifies it.
	ID string `json:"id"`
}

// BundleChain declares how the claims are linked and the newest coordinates the producer attests.
type BundleChain struct {
	// Profile names the link construction.
	Profile string `json:"profile"`
	// Keyed says whether recomputing a link needs a secret. The audit profile is unkeyed, so this is
	// always false here, but the schema requires the member to be present rather than inferred: a
	// reader must not have to know a profile's properties to know whether it can verify the chain.
	Keyed bool `json:"keyed"`
	// Head is the newest coordinate attested for the whole chain, which may lead the bundled claims
	// when a bundle is a window into a longer history.
	Head BundleCoord `json:"head"`
}

// BundleCoord is a position in a chain.
type BundleCoord struct {
	// Seq is the position, starting at one.
	Seq int64 `json:"seq"`
	// Link is the hash committing to that position and everything before it.
	Link string `json:"link"`
}

// BundleClaim is one audit entry as a claim.
type BundleClaim struct {
	// Type names the payload shape.
	Type string `json:"type"`
	// At is when the recorded mutation happened.
	At string `json:"at"`
	// Payload carries the claim's fields. The registry's minimum for this type is actor, method,
	// and path, which are exactly the fields the chain link commits to.
	Payload map[string]any `json:"payload"`
	// Chain is the claim's position and links.
	Chain BundleCoordLink `json:"chain"`
}

// BundleCoordLink is a claim's chain coordinates.
type BundleCoordLink struct {
	// Seq is the claim's position.
	Seq int64 `json:"seq"`
	// Prev is the previous claim's link, empty for the genesis claim.
	Prev string `json:"prev"`
	// Link is this claim's link.
	Link string `json:"link"`
}

// BuildBundle assembles entries into an unsigned bundle. Entries must be the chain in ascending
// sequence order, contiguous, which is how the store returns them.
//
// The claim's at is written in the same form the chain link hashes, so a verifier that normalizes it
// gets back exactly the string the link was computed over.
func BuildBundle(entries []*Entry, id Identity, version string, at time.Time) (*Bundle, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("%w: no entries to bundle", ErrExport)
	}
	claims := make([]BundleClaim, 0, len(entries))
	for i, e := range entries {
		if e.Seq != entries[0].Seq+int64(i) {
			return nil, fmt.Errorf("%w: entries are not contiguous at sequence %d", ErrExport, e.Seq)
		}
		claims = append(claims, BundleClaim{
			Type: ClaimType,
			At:   e.At.UTC().Format(time.RFC3339Nano),
			Payload: map[string]any{
				"actor":  e.Actor,
				"method": e.Method,
				"path":   e.Path,
			},
			Chain: BundleCoordLink{Seq: e.Seq, Prev: e.PrevHash, Link: e.Hash},
		})
	}
	head := entries[len(entries)-1]
	return &Bundle{
		LoomSeal:  loomsealVersion,
		BundleID:  "lsb_" + head.Hash[:12],
		CreatedAt: at.UTC().Format(time.RFC3339),
		Producer: BundleProducer{
			Product:        ProductName,
			ProductVersion: version,
			InstallID:      id.InstallID,
			PublicKey:      id.PublicKeyBase64(),
			KeyID:          seal.KeyID(id.Public()),
		},
		// The claims are about the fleet this install manages, which is the schema's vocabulary for
		// the thing a controller's changes act on.
		Subject: BundleSubject{Type: "fleet", ID: id.InstallID},
		Chain: &BundleChain{
			Profile: ChainProfile,
			Keyed:   false,
			Head:    BundleCoord{Seq: head.Seq, Link: head.Hash},
		},
		Claims:     claims,
		Signatures: []any{},
	}, nil
}

// SignBundleDoc signs a bundle and returns its canonical signed bytes. Signing is delegated to the
// LoomSeal seal package so the canonicalization a signature covers is the verifier's own, never a
// second implementation of it that could drift.
func SignBundleDoc(b *Bundle, priv ed25519.PrivateKey) ([]byte, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("%w: encode bundle: %w", ErrExport, err)
	}
	signed, err := seal.SignBundle(raw, priv)
	if err != nil {
		return nil, fmt.Errorf("%w: sign bundle: %w", ErrExport, err)
	}
	return signed, nil
}
