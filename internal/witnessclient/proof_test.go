package witnessclient

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/kordloom/loomseal/merkle"

	"github.com/kordloom/switchtender/internal/audit"
)

// TestProofMatchesTheMerklePackage holds the tree the client folds from leaf hashes to the merkle
// package's own over the leaves: every root equals merkle.Root, every consistency proof equals
// merkle.ConsistencyProof byte for byte, and merkle.VerifyConsistency accepts each one.
func TestProofMatchesTheMerklePackage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Size is the tree size every prefix of which is checked.
		Size int
	}{{Size: 1}, {Size: 2}, {Size: 3}, {Size: 7}, {Size: 8}, {Size: 9}, {Size: 16}, {Size: 17},
		{Size: 33}, {Size: 64}, {Size: 100}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			leaves := make([][]byte, test.Size)
			hashes := make([][merkle.HashSize]byte, test.Size)
			for i := range leaves {
				leaves[i] = fmt.Appendf(nil, "leaf %d of %d", i, test.Size)
				hashes[i] = [merkle.HashSize]byte(merkle.LeafHash(leaves[i]))
			}
			n := int64(test.Size)
			root := treeRoot(hashes)
			if !bytes.Equal(root, merkle.Root(leaves)) {
				t.Fatalf("treeRoot() over %d leaves differs from merkle.Root", n)
			}
			for m := int64(1); m <= n; m++ {
				got, err := consistencyProof(m, hashes)
				if err != nil {
					t.Fatalf("consistencyProof(%d) error = %v", m, err)
				}
				want, err := merkle.ConsistencyProof(m, leaves)
				if err != nil {
					t.Fatalf("merkle.ConsistencyProof(%d) error = %v", m, err)
				}
				if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("proof from %d to %d mismatch (-merkle +client):\n%s", m, n, diff)
				}
				if !merkle.VerifyConsistency(m, n, treeRoot(hashes[:m]), root, got) {
					t.Errorf("the proof from %d to %d does not verify", m, n)
				}
			}
			if got, err := consistencyProof(0, hashes); err != nil || len(got) != 0 {
				t.Errorf("consistencyProof(0) = %v, %v; want no proof for a witness holding none",
					got, err)
			}
			if _, err := consistencyProof(n+1, hashes); !errors.Is(err, ErrChain) {
				t.Errorf("consistencyProof(%d) over %d = %v, want ErrChain", n+1, n, err)
			}
		})
	}
}

// TestLeafCacheProofMatchesTheChainTreeHead holds the cache to the chain's own tree: read in pieces
// or whole, its root at every size is audit.TreeHead's over the same entries, so a pushed root is
// the one a bundle and an anchor compute. A beat whose link is not the chain's is refused and the
// cache starts over.
func TestLeafCacheProofMatchesTheChainTreeHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in := newTestInstall(t)
	in.entries(t, 3)
	b := in.beat(t)
	var lc leafCache
	if err := lc.extend(ctx, in.store, in.id.InstallID, b.Seq, b.Hash); err != nil {
		t.Fatalf("extend() error = %v", err)
	}
	in.entries(t, 5)
	b = in.beat(t)
	in.entries(t, 2)
	if err := lc.extend(ctx, in.store, in.id.InstallID, b.Seq, b.Hash); err != nil {
		t.Fatalf("extend() error = %v", err)
	}
	chain, err := in.store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if lc.size() != int64(len(chain)) {
		t.Fatalf("the cache holds %d entries, the chain %d", lc.size(), len(chain))
	}
	var whole leafCache
	if err := whole.extend(ctx, in.store, in.id.InstallID, 0, ""); err != nil {
		t.Fatalf("extend() whole error = %v", err)
	}
	if diff := cmp.Diff(whole.hashes, lc.hashes); diff != "" {
		t.Errorf("read in pieces and whole differ (-whole +pieces):\n%s", diff)
	}
	for n := 1; n <= len(chain); n++ {
		size, root, err := audit.TreeHead(chain[:n], in.id.InstallID)
		if err != nil {
			t.Fatalf("TreeHead() error = %v", err)
		}
		if got := hex.EncodeToString(treeRoot(lc.hashes[:n])); size != int64(n) || got != root {
			t.Errorf("root at size %d = %s, TreeHead says %s at %d", n, got, root, size)
		}
	}
	if _, err := audit.LeafHash(nil, in.id.InstallID); err == nil {
		t.Error("LeafHash(nil) succeeded, want a refusal")
	}

	in.entries(t, 1)
	forged := in.beat(t)
	if err := lc.extend(ctx, in.store, in.id.InstallID, forged.Seq, b.Hash); !errors.Is(err,
		ErrChain) || lc.size() != 0 {
		t.Errorf("extend() over a beat naming another link = %v with %d cached, want ErrChain "+
			"and an empty cache", err, lc.size())
	}
	if err := lc.extend(ctx, in.store, in.id.InstallID, forged.Seq+5, ""); !errors.Is(err,
		ErrChain) {
		t.Errorf("extend() for a beat past the chain = %v, want ErrChain", err)
	}
}
