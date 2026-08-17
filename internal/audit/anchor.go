package audit

import (
	"context"
	"errors"
	"time"

	"github.com/kordloom/switchtender/internal/idgen"
)

// Anchor types, matching the LoomSeal format's anchor table. Each fixes a chain link in a place the
// producer cannot quietly rewrite, and they differ in how a relying party checks that.
const (
	// AnchorRFC3161 is a timestamp authority's signed token over the link. It is the only type that
	// carries its own offline proof, so a relying party checks it without fetching anything.
	AnchorRFC3161 = "rfc3161"
	// AnchorGit is a commit in a repository the producer does not control alone. A relying party
	// checks it by fetching the commit.
	AnchorGit = "git"
	// AnchorHTTPS is a published head at a URL, checked by retrieval.
	AnchorHTTPS = "https"
)

// Anchor shapes, the coordinate space an anchor's Seq and Link live in. A linear anchor and a tree
// anchor are checked in entirely different ways, so the shape is persisted with the anchor rather
// than guessed from its values, which are both hex strings.
const (
	// AnchorShapeLinear fixes a chain position: Seq is an entry's sequence and Link is that
	// entry's hash. It is checked by finding the entry and comparing hashes.
	AnchorShapeLinear = "linear"
	// AnchorShapeTree fixes a Merkle coordinate: Seq is a tree size and Link is the root over the
	// first Seq entries. It is checked by recomputing that tree, never against an entry hash.
	AnchorShapeTree = "tree"
)

// ErrAnchorType is returned for an anchor type the format does not define.
var ErrAnchorType = errors.New("unknown anchor type")

// ErrAnchorShape is returned for an anchor shape the format does not define.
var ErrAnchorShape = errors.New("unknown anchor shape")

// ErrAnchorNotFound is returned when a named anchor does not exist.
var ErrAnchorNotFound = errors.New("anchor not found")

// Anchor fixes one chain link in time somewhere outside this install.
//
// The chain proves nothing was altered. It cannot prove nothing was removed from the end, because a
// prefix of a valid chain is itself a valid chain: drop the last thousand entries and what remains
// verifies perfectly. An anchor is what closes that. Once a link is recorded somewhere the operator
// cannot rewrite alone, a chain that no longer reaches it has visibly lost its tail.
type Anchor struct {
	// ID is the unique anchor identifier.
	ID string `json:"id"`
	// Type is the anchor kind: rfc3161, git, or https.
	Type string `json:"type"`
	// Shape is the coordinate space Seq and Link live in: linear for an entry hash at a chain
	// position, tree for a Merkle root at a tree size.
	Shape string `json:"shape"`
	// Seq is the coordinate's position: a chain sequence for a linear anchor, a tree size for a
	// tree anchor.
	Seq int64 `json:"seq"`
	// Link is the value being anchored: the entry hash at Seq for a linear anchor, the Merkle root
	// over the first Seq entries for a tree anchor.
	Link string `json:"link"`
	// At is when the anchor was made.
	At time.Time `json:"at"`
	// Ref locates the anchor: a timestamp authority URL, a commit URL, or a published head URL.
	Ref string `json:"ref"`
	// Proof is an embedded offline proof, base64, carried only by rfc3161 anchors. Empty means a
	// relying party checks the anchor by fetching Ref instead.
	Proof string `json:"proof,omitempty"`
	// InstallID is the install whose identity the anchored value was computed under. A tree anchor
	// fixes a Merkle root whose leaves are bound to that identity, so the same chain under a different
	// identity recomputes a different root, and the check has no way to tell that apart from a rewrite
	// without knowing which install took the anchor. Empty on an anchor recorded before this was kept.
	InstallID string `json:"install_id,omitempty"`
}

// AnchorStore persists anchors. Implementations must be safe for concurrent use.
type AnchorStore interface {
	// SaveAnchor records one anchor.
	SaveAnchor(ctx context.Context, a *Anchor) error
	// Anchors returns every anchor at or below seq, oldest first, so a bundle carries the anchors
	// that actually cover the range it holds. A seq of zero or less returns all of them.
	Anchors(ctx context.Context, seq int64) ([]*Anchor, error)
	// DeleteAnchor removes the anchor with the given id, so one recorded over the wrong
	// coordinates can be withdrawn rather than failing every export forever. It returns
	// ErrAnchorNotFound when no anchor carries that id.
	DeleteAnchor(ctx context.Context, id string) error
}

// NewAnchorID returns a random anchor identifier prefixed with "anc_".
func NewAnchorID() string { return idgen.New("anc_", 6) }

// ValidAnchorType reports whether t is an anchor type the format defines.
func ValidAnchorType(t string) bool {
	switch t {
	case AnchorRFC3161, AnchorGit, AnchorHTTPS:
		return true
	default:
		return false
	}
}

// ValidAnchorShape reports whether s is an anchor shape the format defines.
func ValidAnchorShape(s string) bool {
	return s == AnchorShapeLinear || s == AnchorShapeTree
}
