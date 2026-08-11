package audit

import (
	"encoding/hex"
	"fmt"
	"time"

	"github.com/kordloom/loomseal/jcs"
	"github.com/kordloom/loomseal/merkle"
	"github.com/kordloom/loomseal/seal"
)

// TreeProfile names the chain construction a sparse bundle uses: an RFC 6962 Merkle tree, where a
// claim is proved to belong to the log by an audit path rather than by its neighbors.
const TreeProfile = "loomseal-merkle-v1"

// TreeHead returns the size and root of the Merkle tree over the whole chain, the coordinate an
// anchor fixes for the tree profile.
//
// Anchoring a root rather than a link is what makes truncation refutable instead of merely suspicious.
// A linear anchor says the chain once reached a certain link, so losing a tail shows up as a chain
// that can no longer reach its anchor. A root anchored at a size, paired with a later consistency
// proof from that same root, says something stronger: the log the world saw is provably a prefix of
// the log there is now, so an entry recorded before the anchor cannot have been changed or dropped.
func TreeHead(entries []*Entry, installID string) (int64, string, error) {
	if len(entries) == 0 {
		return 0, "", fmt.Errorf("%w: no entries to hash", ErrExport)
	}
	leaves, err := treeLeaves(entries, installID)
	if err != nil {
		return 0, "", err
	}
	return int64(len(leaves)), hex.EncodeToString(merkle.Root(leaves)), nil
}

// treeLeaves hashes every entry of a chain into its leaf bytes, in order. It is the one place the log
// becomes a tree, so a root computed for an anchor and a root computed for a receipt cannot differ.
func treeLeaves(entries []*Entry, installID string) ([][]byte, error) {
	leaves := make([][]byte, 0, len(entries))
	for _, e := range entries {
		leaf, err := treeLeaf(claimContent(e), installID)
		if err != nil {
			return nil, fmt.Errorf("%w: entry %d: %w", ErrExport, e.Seq, err)
		}
		leaves = append(leaves, leaf)
	}
	return leaves, nil
}

// BuildTreeBundle assembles a bundle that discloses only the named entries, proving each belongs to
// the whole chain without carrying the entries around it.
//
// This is what a linear bundle cannot do. A hash chain links every entry to the one before it, so
// proving an entry is in the chain means shipping the run of entries that reaches it, and on a busy
// install that run is mostly other people's work. Here the whole chain is hashed into a tree, the
// root is signed, and each disclosed entry travels with the sibling hashes on its path to that root.
// The siblings are opaque, so a receipt about one run says nothing about what else the log holds.
//
// all must be the complete chain in ascending sequence, which is how the store returns it, because
// the tree is over the log rather than over the disclosure. disclose names the sequences to include.
func BuildTreeBundle(all []*Entry, disclose map[int64]bool, id Identity, version string,
	subject BundleSubject, at time.Time) (*Bundle, error) {
	if len(all) == 0 {
		return nil, fmt.Errorf("%w: no entries to bundle", ErrExport)
	}
	if len(disclose) == 0 {
		return nil, fmt.Errorf("%w: a bundle discloses at least one entry", ErrExport)
	}
	// The chain is verified before anything is built from it, so a bundle is never published from a
	// chain that does not hold together. A tree built over a broken chain would still verify against
	// its own root, which is exactly the reassuring lie this refuses to emit.
	if ok, brokeAt := Verify(all); !ok {
		return nil, fmt.Errorf("%w: the chain does not verify at entry %d, so a bundle built from "+
			"it would attest a history this install cannot stand behind", ErrExport, brokeAt)
	}

	leaves, err := treeLeaves(all, id.InstallID)
	if err != nil {
		return nil, err
	}
	claims := make([]BundleClaim, 0, len(disclose))
	root := merkle.Root(leaves)

	// Disclosed claims are emitted in chain order, which the tree profile requires and which keeps a
	// receipt readable as a sequence of events.
	for i, e := range all {
		if !disclose[e.Seq] {
			continue
		}
		path, err := merkle.InclusionProof(int64(i), leaves)
		if err != nil {
			return nil, fmt.Errorf("%w: entry %d: %w", ErrExport, e.Seq, err)
		}
		claim := claimContent(e)
		// A tree claim carries its leaf index and its own leaf hash. There is no previous link,
		// because a tree has no per-entry predecessor.
		claim.Chain = BundleCoordLink{Seq: int64(i + 1), Link: hex.EncodeToString(merkle.LeafHash(leaves[i]))}
		claim.Inclusion = &BundleInclusion{Path: hexHashes(path)}
		claims = append(claims, claim)
	}
	if len(claims) == 0 {
		return nil, fmt.Errorf("%w: none of the named entries are in this chain", ErrExport)
	}

	return &Bundle{
		LoomSeal:  loomsealVersion,
		BundleID:  "lsb_" + hex.EncodeToString(root)[:12],
		CreatedAt: at.UTC().Format(time.RFC3339),
		Producer: BundleProducer{
			Product:        ProductName,
			ProductVersion: version,
			InstallID:      id.InstallID,
			PublicKey:      id.PublicKeyBase64(),
			KeyID:          seal.KeyID(id.Public()),
		},
		Subject: subject,
		Chain: &BundleChain{
			Profile: TreeProfile,
			Keyed:   false,
			// The install is hashed into every leaf and must be the signer's own, so a second producer
			// cannot re-sign this log's leaves, root, and anchor as its own history.
			Params: map[string]string{"install_id": id.InstallID},
			Head:   BundleCoord{Seq: int64(len(leaves)), Link: hex.EncodeToString(root)},
		},
		Claims:     claims,
		Signatures: []any{},
	}, nil
}

// AttachConsistency proves the log this bundle heads grew from an earlier root by appending only, so
// a reader holding that earlier root learns nothing it covered was changed or dropped.
//
// The earlier root is one somebody already saw: an anchor, a published head, a receipt they hold. A
// proof from a root only this install ever knew shows nothing, since the producer chose both ends.
func (b *Bundle) AttachConsistency(all []*Entry, fromSize int64, id Identity) error {
	if b.Chain == nil || b.Chain.Profile != TreeProfile {
		return fmt.Errorf("%w: a consistency proof belongs to the tree profile", ErrExport)
	}
	if fromSize < 1 || fromSize > int64(len(all)) {
		return fmt.Errorf("%w: cannot prove a prefix of %d against a log of %d", ErrExport, fromSize,
			len(all))
	}
	leaves, err := treeLeaves(all, id.InstallID)
	if err != nil {
		return err
	}
	proof, err := merkle.ConsistencyProof(fromSize, leaves)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrExport, err)
	}
	b.Chain.Consistency = &BundleConsistency{
		FromSize: fromSize,
		FromRoot: hex.EncodeToString(merkle.Root(leaves[:fromSize])),
		Path:     hexHashes(proof),
	}
	return nil
}

// treeLeaf builds one claim's leaf bytes for the tree profile: the canonical object naming the
// profile, the install, and a digest of the claim's content. The digest covers the claim without the
// members that describe where it sits or how it is proved, so the same entry yields the same leaf
// however it is later disclosed.
func treeLeaf(claim BundleClaim, installID string) ([]byte, error) {
	content := map[string]any{
		"type":    claim.Type,
		"at":      claim.At,
		"payload": claim.Payload,
	}
	canonical, err := jcs.Serialize(content)
	if err != nil {
		return nil, err
	}
	sum, err := jcs.Serialize(map[string]any{
		"domain":     TreeProfile,
		"install_id": installID,
		"claim":      "sha256:" + merkle.Sum256Hex(canonical),
	})
	if err != nil {
		return nil, err
	}
	return sum, nil
}

// hexHashes encodes a proof's hashes for a bundle.
func hexHashes(in [][]byte) []string {
	out := make([]string, 0, len(in))
	for _, h := range in {
		out = append(out, hex.EncodeToString(h))
	}
	return out
}
