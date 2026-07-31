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

// ErrAnchorType is returned for an anchor type the format does not define.
var ErrAnchorType = errors.New("unknown anchor type")

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
	// Seq is the chain position this anchor fixes.
	Seq int64 `json:"seq"`
	// Link is the hash of the entry at Seq, the value being anchored.
	Link string `json:"link"`
	// At is when the anchor was made.
	At time.Time `json:"at"`
	// Ref locates the anchor: a timestamp authority URL, a commit URL, or a published head URL.
	Ref string `json:"ref"`
	// Proof is an embedded offline proof, base64, carried only by rfc3161 anchors. Empty means a
	// relying party checks the anchor by fetching Ref instead.
	Proof string `json:"proof,omitempty"`
}

// AnchorStore persists anchors. Implementations must be safe for concurrent use.
type AnchorStore interface {
	// SaveAnchor records one anchor.
	SaveAnchor(ctx context.Context, a *Anchor) error
	// Anchors returns every anchor at or below seq, oldest first, so a bundle carries the anchors
	// that actually cover the range it holds. A seq of zero or less returns all of them.
	Anchors(ctx context.Context, seq int64) ([]*Anchor, error)
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
