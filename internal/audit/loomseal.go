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
	// Attestations are head-level counter-signatures other tools may attach. This product never
	// emits them and does not verify them; the field exists so their presence is seen and refused
	// by name rather than silently ignored, which would let an unchecked rider travel inside a
	// green verdict.
	Attestations []json.RawMessage `json:"attestations,omitempty"`
	// Producer says who emitted the bundle and which key signed it.
	Producer BundleProducer `json:"producer"`
	// Subject says what the claims are about.
	Subject BundleSubject `json:"subject"`
	// Chain carries the profile and the head the producer attests for the whole chain.
	Chain *BundleChain `json:"chain,omitempty"`
	// Claims are the audit entries, ascending by sequence and contiguous.
	Claims []BundleClaim `json:"claims"`
	// Anchors fix chain links in places this install cannot rewrite alone, so a chain that has
	// silently lost its tail can be caught. Omitted when there are none, which is level 2.
	Anchors []BundleAnchor `json:"anchors,omitempty"`
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
	// PublicKey is the raw ed25519 public key in standard base64, the encoding the bundle format
	// specifies. The trust page publishes its fingerprint so a relying party can pin it.
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
	// Params carries profile parameters, such as the install a tree's leaves bind to.
	Params map[string]string `json:"params,omitempty"`
	// Keyed says whether recomputing a link needs a secret. The audit profile is unkeyed, so this is
	// always false here, but the schema requires the member to be present rather than inferred: a
	// reader must not have to know a profile's properties to know whether it can verify the chain.
	Keyed bool `json:"keyed"`
	// Head is the newest coordinate attested for the whole chain, which may lead the bundled claims
	// when a bundle is a window into a longer history. In the tree profile its Seq is the tree size
	// and its Link is the root.
	Head BundleCoord `json:"head"`
	// Consistency proves the log grew from an earlier root by appending only. Tree profile only.
	Consistency *BundleConsistency `json:"consistency,omitempty"`
}

// BundleConsistency proves that a log of FromSize entries whose root was FromRoot is a prefix of the
// log this bundle heads, so nothing that root covered was changed or dropped.
type BundleConsistency struct {
	// FromSize is the earlier log size.
	FromSize int64 `json:"from_size"`
	// FromRoot is the root at that size.
	FromRoot string `json:"from_root"`
	// Path is the proof's hashes.
	Path []string `json:"path"`
}

// BundleInclusion is a claim's audit path: the sibling hashes, lowest first, that fold with its leaf
// hash to reproduce the root the head names.
type BundleInclusion struct {
	// Path is the audit path, empty for a log of one entry.
	Path []string `json:"path"`
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
	// Disclosures and Attestations are holder- and third-party members other tools may attach.
	// This product never emits either and does not verify them; the fields exist so their
	// presence is refused by name rather than silently ignored.
	Disclosures  []json.RawMessage `json:"disclosures,omitempty"`
	Attestations []json.RawMessage `json:"attestations,omitempty"`
	// Inclusion is the audit path proving this claim belongs to the tree the head names. Tree
	// profile only, and required on every claim there.
	Inclusion *BundleInclusion `json:"inclusion,omitempty"`
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

// BundleAnchor is one external anchor record, in the shape the LoomSeal format defines.
type BundleAnchor struct {
	// Type is the anchor kind: rfc3161, git, or https.
	Type string `json:"type"`
	// Seq is the chain position the anchor fixes.
	Seq int64 `json:"seq"`
	// Link is the hash of the entry at Seq.
	Link string `json:"link"`
	// At is when the anchor was made, RFC 3339 UTC.
	At string `json:"at"`
	// Ref locates the anchor.
	Ref string `json:"ref"`
	// Proof is the embedded offline proof, carried only by rfc3161 anchors.
	Proof string `json:"proof,omitempty"`
}

// AttachAnchors adds the anchor records covering this bundle's range and drops the rest, then
// reports how many were kept.
//
// A verifier rejects a bundle carrying an anchor that names no link the bundle holds, so attaching
// an anchor for a sequence outside the exported range turns a good export into a failing one. The
// rule lives here, beside the format it comes from, rather than at each call site. Anchors must be
// attached before the bundle is signed, since the signature covers them.
//
// The anchor's shape must match the bundle's profile: a linear anchor fixes an entry hash, which a
// tree bundle holds nowhere, and a tree anchor fixes a root, which a linear bundle holds nowhere.
// The coordinate spaces share the same integer positions, so without the shape gate a collision
// between an entry hash and a root would attach an anchor that means something else entirely.
func (b *Bundle) AttachAnchors(anchors []*Anchor) int {
	tree := b.Chain != nil && b.Chain.Profile == TreeProfile
	links := make(map[int64]string, len(b.Claims)+2)
	for _, c := range b.Claims {
		links[c.Chain.Seq] = c.Chain.Link
	}
	if b.Chain != nil {
		links[b.Chain.Head.Seq] = b.Chain.Head.Link
		// An anchor over the root a consistency proof starts from is the pairing that makes truncation
		// refutable rather than merely suspicious: the anchor fixed that root at a time this install
		// did not control, and the proof shows the log there is now grew from exactly it. Dropping such
		// an anchor as matching nothing would discard the strongest evidence the bundle carries.
		if c := b.Chain.Consistency; c != nil {
			links[c.FromSize] = c.FromRoot
		}
	}
	out := make([]BundleAnchor, 0, len(anchors))
	for _, a := range anchors {
		if a == nil || a.Link == "" {
			continue
		}
		if !ValidAnchorShape(a.Shape) || (a.Shape == AnchorShapeTree) != tree {
			continue
		}
		// The two-value read matters: comparing against a map miss let an anchor name a sequence the
		// bundle does not hold, which is the export-rejected-by-the-auditor case this prevents.
		if link, ok := links[a.Seq]; !ok || link != a.Link {
			continue
		}
		out = append(out, BundleAnchor{
			Type: a.Type, Seq: a.Seq, Link: a.Link,
			At: a.At.UTC().Format(time.RFC3339), Ref: a.Ref, Proof: a.Proof,
		})
	}
	if len(out) == 0 {
		b.Anchors = nil
		return 0
	}
	b.Anchors = out
	return len(out)
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
	// The range is verified before anything is built from it, rather than checked for contiguous
	// sequence numbers alone.
	//
	// Arithmetic contiguity says only that the sequence numbers count up. It accepted a reordered
	// chain whose links no longer name the entry before them, and a genesis claim carrying a
	// previous link, both of which the reference verifier rejects. A bundle that builds here and
	// fails at the auditor is worse than one that never builds, because the claim it makes about
	// this install has already been handed over by then.
	if ok, brokeAt := VerifyRange(entries); !ok {
		return nil, fmt.Errorf("%w: the chain does not verify at entry %d, sequence %d, so a bundle "+
			"built from it would be rejected by any verifier. GET /v1/audit/verify reports where",
			ErrExport, brokeAt, entries[brokeAt-1].Seq)
	}
	claims := make([]BundleClaim, 0, len(entries))
	// The newest span beat turned into a claim so far, so each beat's time can be checked against
	// the one before it while the claims are built.
	var prevSpanAt time.Time
	var prevSpanBeat int64
	for _, e := range entries {
		// An entry recorded before times were truncated carries nanoseconds, and a verifier that
		// normalizes through microseconds recomputes a different link. Such a bundle verifies here
		// and fails at the auditor, which is the worst place to find out, so it is refused at build
		// time with the reason. The entry cannot be repaired: its hash commits to the time it holds.
		if e.At.Nanosecond()%1000 != 0 {
			return nil, fmt.Errorf(
				"%w: entry %d was recorded at nanosecond precision, before this was corrected, so "+
					"its link cannot be recomputed by a verifier that carries time at microsecond "+
					"precision. Bundle a later range with --limit, or re-export once the chain has "+
					"advanced past it", ErrExport, e.Seq)
		}
		claim := claimContent(e)
		claim.Chain = BundleCoordLink{Seq: e.Seq, Prev: e.PrevHash, Link: e.Hash}
		// A span beat becomes the spec-owned span claim, so a verifier reads the beat stream
		// without knowing this product's path encoding. The span members are added to the payload
		// rather than replacing it: the chain link commits to actor, method, and path, and the
		// profile promises every link recomputes from the claim payload alone, so dropping them
		// left every span claim's link unrecomputable by anyone holding only the bundle. The
		// registry fixes payload minimums, not maximums, so the extra members are still a valid
		// span claim. The chain coordinates stay exactly as stored, and a span-marked entry whose
		// path does not round-trip stays a generic claim rather than becoming a malformed span one.
		if e.Actor == SpanActor && e.Method == SpanMethod {
			if beat, _, _, ok := ParseSpanPath(e.Path); ok {
				// A verifier fails a bundle whose beat time does not strictly advance past the beat
				// before it, where a cadence gap is only reported. Neither entry of such a pair can
				// be repaired, since each link commits to the time it holds, so a chain poisoned by
				// an older build that wrote a beat behind its predecessor is caught here rather
				// than at the auditor. See CheckBeatAdvance for how a backward clock is kept from
				// making one now: the beat is skipped, not moved.
				if err := checkSpanAdvance(prevSpanAt, prevSpanBeat, e.At, beat); err != nil {
					return nil, err
				}
				prevSpanAt, prevSpanBeat = e.At, beat
			}
		}
		claims = append(claims, claim)
	}
	head := entries[len(entries)-1]
	// A blank head hash is exactly the tamper the chain exists to catch, so it must not take the
	// export down on the way to reporting it. Slicing a hash that was blanked out panicked instead.
	if len(head.Hash) < 12 {
		return nil, fmt.Errorf("%w: entry %d has no hash, so the chain is broken and cannot be "+
			"bundled. GET /v1/audit/verify reports where", ErrExport, head.Seq)
	}
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
			// Stated for a reader, not relied on. The binding that matters is per entry: each
			// claim written since the binding existed carries its own install_id, folded into its
			// link, so a copier who rewrites the producer breaks the equality check and one who
			// rewrites the claim's id too breaks the link. A chain-level value could not do that
			// job, because it applies to every claim at once and a chain spanning the upgrade holds
			// both bound and pre-binding entries.
			Params: map[string]string{"install_id": id.InstallID},
			Head:   BundleCoord{Seq: head.Seq, Link: head.Hash},
		},
		Claims:     claims,
		Signatures: []any{},
	}, nil
}

// claimContent builds the type, time, and payload of a bundle claim from an entry: everything except
// the chain coordinates, which are the only part that differs between chain profiles.
//
// Sharing one definition is what keeps a claim's content, and therefore the digest a tree leaf commits
// to, identical whichever shape carries it. Two builders that drifted would produce a receipt whose
// leaves no longer recompute against the root the producer signed.
func claimContent(e *Entry) BundleClaim {
	claim := BundleClaim{
		Type: ClaimType,
		At:   e.At.UTC().Format(time.RFC3339Nano),
		Payload: map[string]any{
			"actor":  e.Actor,
			"method": e.Method,
			"path":   e.Path,
		},
	}
	// The profile promises every link recomputes from the claim payload alone, so a field the link
	// commits to must appear here. These are omitted when empty, exactly as the link omits them, so an
	// entry that predates a field carries the same payload it always did.
	for key, value := range map[string]string{
		"actor_type":     e.ActorType,
		"on_behalf_of":   e.OnBehalfOf,
		"content_digest": e.ContentDigest,
		"install_id":     e.InstallID,
	} {
		if value != "" {
			claim.Payload[key] = value
		}
	}
	// A span beat becomes the spec-owned span claim, so a verifier reads the beat stream without
	// knowing this product's path encoding. The span members are added to the payload rather than
	// replacing it, because the chain link commits to actor, method, and path and the profile promises
	// every link recomputes from the payload alone.
	if e.Actor == SpanActor && e.Method == SpanMethod {
		if beat, count, cadenceS, ok := ParseSpanPath(e.Path); ok {
			claim.Type = SpanClaimType
			claim.Payload["stream"] = "chain"
			claim.Payload["cadence_s"] = int64(cadenceS)
			claim.Payload["beat"] = beat
			claim.Payload["count"] = count
		}
	}
	return claim
}

// checkSpanAdvance reports why a bundle cannot be built when a beat's time does not advance past
// the beat before it. A zero prevAt means this is the first beat in the range, which has nothing to
// advance past.
func checkSpanAdvance(prevAt time.Time, prevBeat int64, at time.Time, beat int64) error {
	if prevAt.IsZero() || at.After(prevAt) {
		return nil
	}
	return fmt.Errorf("%w: span beat %d at %s does not advance past beat %d at %s, so every verifier "+
		"rejects a bundle carrying both. Neither entry can be repaired, since its link commits to the "+
		"time it holds. Bundle a later range with --limit, or re-export once the chain has advanced "+
		"past them", ErrExport, beat, at.UTC().Format(time.RFC3339Nano), prevBeat,
		prevAt.UTC().Format(time.RFC3339Nano))
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
