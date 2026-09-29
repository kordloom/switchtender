package audit

import (
	"encoding/hex"

	"github.com/kordloom/loomseal/merkle"
)

// ChainScanner verifies a chain fed one entry at a time in chain order, holding only the previous
// entry, so a caller can check a trail of any length in constant memory. Feed every entry, then
// read Result. VerifyRange and Verify are the slice forms of the same walk.
type ChainScanner struct {
	// fromGenesis requires the first entry fed to open the chain: sequence one, no previous link.
	fromGenesis bool
	// prev is the last sound entry, nil before the first.
	prev *Entry
	// count is how many entries were fed.
	count int
	// brokeAt is the one-based position of the first entry that did not check out, zero while the
	// chain holds.
	brokeAt int
}

// NewChainScanner returns a scanner. fromGenesis requires the first entry to open the chain,
// which is Verify's rule; without it the walk accepts any contiguous range, which is VerifyRange's.
func NewChainScanner(fromGenesis bool) *ChainScanner {
	return &ChainScanner{fromGenesis: fromGenesis}
}

// Feed checks the next entry in chain order. Once the chain has broken, further entries are
// counted but not checked, so Result still names the first break.
func (v *ChainScanner) Feed(e *Entry) {
	v.count++
	if v.brokeAt != 0 {
		return
	}
	// A null entry is a broken chain, not a crash: fed slices are decoded from documents handed
	// over by whoever is being audited.
	if e == nil || e.Hash == "" || e.Seq < 1 || e.Hash != EntryHash(e) {
		v.brokeAt = v.count
		return
	}
	switch {
	case v.prev == nil && v.fromGenesis:
		if e.Seq != 1 || e.PrevHash != "" {
			v.brokeAt = v.count
			return
		}
	case v.prev == nil:
		// The first entry of a range still constrains: sequence one is the one entry in a chain
		// with nothing before it, so it must carry no previous link, and any other first entry
		// must carry one.
		if (e.Seq == 1) != (e.PrevHash == "") {
			v.brokeAt = v.count
			return
		}
	default:
		if e.Seq != v.prev.Seq+1 || e.PrevHash != v.prev.Hash {
			v.brokeAt = v.count
			return
		}
	}
	v.prev = e
}

// Result reports whether every entry fed so far checked out and, when one did not, the one-based
// position of the first break. Count is how many entries were fed.
func (v *ChainScanner) Result() (ok bool, brokeAt, count int) {
	return v.brokeAt == 0, v.brokeAt, v.count
}

// AnchorScanner holds a chain against its anchors while the chain streams past. A linear anchor
// needs only the hash at its position, so those cost memory in the number of anchors. A tree
// anchor fixes a Merkle root at a tree size, which no single entry carries, so the scanner also
// folds every streamed entry into an incremental tree and captures the root at each anchored
// size, costing a logarithm of the chain length on top. CheckAnchors is the slice form of the
// same walk.
type AnchorScanner struct {
	// anchors is the set being checked, in the order results are reported.
	anchors []*Anchor
	// installID is the install the tree's leaves bind to, the same value TreeHead hashes. It is
	// only consulted when a tree anchor is present.
	installID string
	// wanted marks the linear anchored sequences still worth capturing.
	wanted map[int64]struct{}
	// atSeq is the link found at each linear anchored sequence.
	atSeq map[int64]string
	// treeSizes marks the tree sizes an anchor fixes a root at.
	treeSizes map[int64]struct{}
	// rootAt is the recomputed root at each anchored tree size.
	rootAt map[int64]string
	// forest holds the incremental tree as perfect subtree roots, indexed by level, non-nil where
	// the matching bit of fed is set. It is the standard compact form of an RFC 6962 tree.
	forest [][]byte
	// fed is how many entries the tree has folded in, which is the current tree size.
	fed int64
	// treeErr is the first failure turning an entry into a leaf, which makes every tree anchor
	// uncheckable rather than silently unreached.
	treeErr error
	// highest is the largest sequence seen.
	highest int64
}

// NewAnchorScanner returns a scanner over the given anchors. installID is the install the tree
// profile's leaves bind to; it matters only when a tree anchor is among them.
func NewAnchorScanner(anchors []*Anchor, installID string) *AnchorScanner {
	s := &AnchorScanner{
		anchors:   anchors,
		installID: installID,
		wanted:    make(map[int64]struct{}, len(anchors)),
		atSeq:     make(map[int64]string, len(anchors)),
		treeSizes: make(map[int64]struct{}),
		rootAt:    make(map[int64]string),
	}
	for _, a := range anchors {
		if a.Shape == AnchorShapeTree {
			s.treeSizes[a.Seq] = struct{}{}
			continue
		}
		s.wanted[a.Seq] = struct{}{}
	}
	return s
}

// Feed records what the entry proves about the anchors: the link at a linear anchored position,
// the tree root at an anchored size, and how far the chain reaches.
func (s *AnchorScanner) Feed(e *Entry) {
	if e == nil {
		return
	}
	if e.Seq > s.highest {
		s.highest = e.Seq
	}
	if _, ok := s.wanted[e.Seq]; ok {
		s.atSeq[e.Seq] = e.Hash
	}
	if len(s.treeSizes) == 0 || s.treeErr != nil || s.installID == "" {
		return
	}
	leaf, err := treeLeaf(claimContent(e), s.installID)
	if err != nil {
		s.treeErr = err
		return
	}
	s.pushLeaf(merkle.LeafHash(leaf))
	s.fed++
	if _, ok := s.treeSizes[s.fed]; ok {
		s.rootAt[s.fed] = hex.EncodeToString(s.forestRoot())
	}
}

// pushLeaf folds one leaf hash into the forest, merging equal-size perfect subtrees like a binary
// carry. Node hashing is the reference implementation's, so the root here is TreeHead's root.
func (s *AnchorScanner) pushLeaf(h []byte) {
	for i := 0; ; i++ {
		if i == len(s.forest) {
			s.forest = append(s.forest, nil)
		}
		if s.forest[i] == nil {
			s.forest[i] = h
			return
		}
		h = merkle.NodeHash(s.forest[i], h)
		s.forest[i] = nil
	}
}

// forestRoot folds the forest's subtree roots, smallest first, into the RFC 6962 root over every
// leaf pushed so far. It does not consume the forest, so the scan continues past it.
func (s *AnchorScanner) forestRoot() []byte {
	var root []byte
	for _, sub := range s.forest {
		if sub == nil {
			continue
		}
		if root == nil {
			root = sub
			continue
		}
		root = merkle.NodeHash(sub, root)
	}
	return root
}

// Results reports what each anchor proves against the chain fed so far, with the same verdicts and
// wording as CheckAnchors.
func (s *AnchorScanner) Results() (ok bool, results []AnchorCheck) {
	return anchorVerdicts(s)
}
