package witnessclient

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/kordloom/loomseal/merkle"

	"github.com/kordloom/switchtender/internal/audit"
)

// A push carries the tree root at the beat's size and a consistency proof from the size the witness
// last acknowledged. Both are RFC 6962 constructions over the chain's leaves. The merkle package
// computes them from the leaves themselves, which would mean holding every leaf's bytes, so the
// client holds each entry's 32 byte leaf hash instead and folds the same tree from those: Root of a
// one-leaf tree is that leaf's hash, and every interior node is merkle.NodeHash of its children
// either way. The tests hold treeRoot and consistencyProof to merkle.Root and
// merkle.ConsistencyProof byte for byte, and every proof to merkle.VerifyConsistency.

// Chain is the part of an audit store the client reads: every entry after a sequence, in order.
type Chain interface {
	// ChainScan streams every entry with a sequence above afterSeq in chain order, oldest first.
	ChainScan(ctx context.Context, afterSeq int64, fn func(*audit.Entry) error) error
}

// leafCache holds the leaf hash of every chain entry the client has read, in chain order, so each
// beat reads only the entries appended since the last one.
type leafCache struct {
	// hashes holds the leaf hash of the entry at sequence i+1 at index i.
	hashes [][merkle.HashSize]byte
	// lastLink is the chain link of the newest cached entry, which the next entry must name as the
	// one before it.
	lastLink string
}

// size returns how many entries the cache holds.
func (lc *leafCache) size() int64 { return int64(len(lc.hashes)) }

// reset empties the cache, so the next extend reads the chain from its first entry.
func (lc *leafCache) reset() {
	lc.hashes, lc.lastLink = nil, ""
}

// extend reads every entry appended after the newest cached one and caches its leaf hash under
// installID. Each entry must sit at the next position and name the newest cached link as the one
// before it, and the entry at seq must carry link, so the tree the client pushes is the chain the
// beat was appended to. A chain that breaks either rule empties the cache, which is read again in
// full on the next beat. A seq of zero checks no beat.
func (lc *leafCache) extend(ctx context.Context, chain Chain, installID string, seq int64,
	link string) error {
	err := chain.ChainScan(ctx, lc.size(), func(e *audit.Entry) error {
		next := lc.size() + 1
		if e.Seq != next {
			return fmt.Errorf("%w: the chain holds entry %d where entry %d belongs", ErrChain, e.Seq,
				next)
		}
		if next > 1 && e.PrevHash != lc.lastLink {
			return fmt.Errorf("%w: entry %d does not follow the entry before it", ErrChain, e.Seq)
		}
		if e.Seq == seq && e.Hash != link {
			return fmt.Errorf("%w: the beat at entry %d names link %s, and the chain holds %s there",
				ErrChain, seq, link, e.Hash)
		}
		leaf, err := audit.LeafHash(e, installID)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrChain, err)
		}
		lc.hashes = append(lc.hashes, [merkle.HashSize]byte(leaf))
		lc.lastLink = e.Hash
		return nil
	})
	if errors.Is(err, ErrChain) {
		lc.reset()
		return err
	}
	if err != nil {
		return fmt.Errorf("read the audit chain: %w", err)
	}
	if seq > lc.size() {
		return fmt.Errorf("%w: the beat is at entry %d and the chain holds %d entries", ErrChain, seq,
			lc.size())
	}
	return nil
}

// treeRoot returns the RFC 6962 root of the tree whose leaf hashes are hashes, the value
// merkle.Root gives over the leaves themselves.
func treeRoot(hashes [][merkle.HashSize]byte) []byte {
	switch len(hashes) {
	case 0:
		return merkle.EmptyRoot()
	case 1:
		return hashes[0][:]
	}
	k := splitPoint(int64(len(hashes)))
	return merkle.NodeHash(treeRoot(hashes[:k]), treeRoot(hashes[k:]))
}

// consistencyProof returns the RFC 6962 proof that the tree over the first first leaves is a prefix
// of the tree over all of hashes, the value merkle.ConsistencyProof gives over the leaves
// themselves. A first of zero is a witness that holds no tree yet, which needs no proof.
func consistencyProof(first int64, hashes [][merkle.HashSize]byte) ([][]byte, error) {
	n := int64(len(hashes))
	if first < 0 || first > n {
		return nil, fmt.Errorf("%w: cannot prove a prefix of %d against a tree of %d", ErrChain,
			first, n)
	}
	if first == 0 || first == n {
		return [][]byte{}, nil
	}
	return subProof(first, hashes, true), nil
}

// subProof is SUBPROOF of RFC 6962 section 2.1.2 over leaf hashes. complete says the prefix is
// exactly the subtree being described, so its root is already known to the verifier and is not
// sent.
func subProof(m int64, hashes [][merkle.HashSize]byte, complete bool) [][]byte {
	n := int64(len(hashes))
	if m == n {
		if complete {
			return nil
		}
		return [][]byte{treeRoot(hashes)}
	}
	k := splitPoint(n)
	if m <= k {
		return append(subProof(m, hashes[:k], complete), treeRoot(hashes[k:]))
	}
	return append(subProof(m-k, hashes[k:], false), treeRoot(hashes[:k]))
}

// splitPoint returns the largest power of two strictly less than n, where RFC 6962 divides a tree
// of n leaves. n must be greater than one.
func splitPoint(n int64) int64 {
	k := int64(1)
	for k<<1 < n {
		k <<= 1
	}
	return k
}

// hexHashes encodes a proof's hashes as the push carries them.
func hexHashes(in [][]byte) []string {
	out := make([]string, 0, len(in))
	for _, h := range in {
		out = append(out, hex.EncodeToString(h))
	}
	return out
}
