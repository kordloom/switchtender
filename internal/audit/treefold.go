package audit

import (
	"encoding/hex"

	"github.com/kordloom/loomseal/merkle"
)

// treeFold folds a streaming chain into an incremental Merkle tree under one install identity and
// captures the root at each size an anchor fixes one at.
//
// The scanner keeps a second one only when an anchor was taken under this same key's earlier name.
// A tree leaf commits to the install id, so the same untouched chain under a different name
// produces a different root, and the id derivation was widened from six raw key bytes to a hash of
// the whole key. Most installs store their id on first boot and kept it, but the SWITCHTENDER_AUDIT_KEY
// path keeps no identity file and re-derives it every boot, which is the documented way to run a
// shared postgres chain. Those installs were renamed underneath chains they had already anchored,
// and every anchor over them then refused to recompute, which refuses to sign a bundle.
type treeFold struct {
	// installID is the identity this fold's leaves bind to.
	installID string
	// forest holds the incremental tree as perfect subtree roots, indexed by level, non-nil where
	// the matching bit of fed is set. It is the standard compact form of an RFC 6962 tree.
	forest [][]byte
	// fed is how many entries this fold has taken in, which is its current tree size.
	fed int64
	// rootAt is the recomputed root at each anchored tree size.
	rootAt map[int64]string
	// err is the first failure turning an entry into a leaf, which stops this fold answering rather
	// than letting it answer wrongly.
	err error
}

// newTreeFold returns a fold binding its leaves to installID.
func newTreeFold(installID string) *treeFold {
	return &treeFold{installID: installID, rootAt: make(map[int64]string)}
}

// feed folds one entry in and records the root when the new size is one an anchor fixes.
func (f *treeFold) feed(e *Entry, sizes map[int64]struct{}) {
	if f.err != nil {
		return
	}
	leaf, err := treeLeaf(claimContent(e), f.installID)
	if err != nil {
		f.err = err
		return
	}
	f.push(merkle.LeafHash(leaf))
	f.fed++
	if _, ok := sizes[f.fed]; ok {
		f.rootAt[f.fed] = hex.EncodeToString(f.root())
	}
}

// push folds one leaf hash into the forest, merging equal-size perfect subtrees like a binary carry.
// Node hashing is the reference implementation's, so the root here is TreeHead's root.
func (f *treeFold) push(h []byte) {
	for i := 0; ; i++ {
		if i == len(f.forest) {
			f.forest = append(f.forest, nil)
		}
		if f.forest[i] == nil {
			f.forest[i] = h
			return
		}
		h = merkle.NodeHash(f.forest[i], h)
		f.forest[i] = nil
	}
}

// root folds the forest's subtree roots, smallest first, into the RFC 6962 root over every leaf
// pushed so far. It does not consume the forest, so the scan continues past it.
func (f *treeFold) root() []byte {
	var root []byte
	for _, sub := range f.forest {
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
