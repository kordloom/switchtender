package audit

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// treeTestInstall is the install id the tree tests bind leaves to.
const treeTestInstall = "inst_anchor_test"

// treeAnchorAt returns a tree anchor fixing the root over the first size entries of chain.
func treeAnchorAt(t *testing.T, chain []*Entry, size int64) *Anchor {
	t.Helper()
	_, root, err := TreeHead(chain[:size], treeTestInstall)
	if err != nil {
		t.Fatalf("TreeHead() error = %v", err)
	}
	return &Anchor{
		ID: fmt.Sprintf("anc_t%d", size), Type: AnchorHTTPS, Shape: AnchorShapeTree,
		Seq: size, Link: root, At: time.Now(), Ref: "https://anchors.example/head",
	}
}

// TestCheckAnchorsRecomputesTreeRootsAtEverySize proves a tree anchor is checked by recomputing
// the Merkle root at its size, at every size from one through the whole chain, so the scanner's
// incremental tree agrees with TreeHead across power-of-two boundaries and everything between.
// Holding a tree anchor against the linear hash map reported every one of these as rewritten.
func TestCheckAnchorsRecomputesTreeRootsAtEverySize(t *testing.T) {
	t.Parallel()
	chain := buildChain(t, 8)
	anchors := make([]*Anchor, 0, len(chain))
	for size := int64(1); size <= int64(len(chain)); size++ {
		anchors = append(anchors, treeAnchorAt(t, chain, size))
	}

	ok, results := CheckAnchors(chain, anchors, treeTestInstall)
	if !ok {
		for _, res := range results {
			if !res.Reached {
				t.Errorf("anchor at size %d unreached: %s", res.Anchor.Seq, res.Problem)
			}
		}
		t.Fatal("an intact chain fails tree anchors recorded over its own roots")
	}

	// The streaming scanner is the form every server path uses, so it must agree entry for entry.
	scan := NewAnchorScanner(anchors, treeTestInstall)
	for _, e := range chain {
		scan.Feed(e)
	}
	if ok, _ := scan.Results(); !ok {
		t.Error("the streaming scanner disagrees with CheckAnchors over the same chain")
	}
}

// TestCheckAnchorsTreeVerdicts covers the failing verdicts a tree anchor can reach: a chain
// shorter than the anchored size is missing entries, a rewritten chain no longer produces the
// anchored root, a missing install identity leaves the anchor uncheckable, and a shape the format
// does not define is refused rather than guessed at.
func TestCheckAnchorsTreeVerdicts(t *testing.T) {
	t.Parallel()
	chain := buildChain(t, 6)
	anchor := treeAnchorAt(t, chain, 6)

	tests := []struct {
		Entries     []*Entry
		Anchor      *Anchor
		InstallID   string
		WantProblem string
	}{{ // Test 0: the chain lost its tail below the anchored size.
		Entries: chain[:4], Anchor: anchor, InstallID: treeTestInstall,
		WantProblem: "2 entries that existed when it was taken are missing",
	}, { // Test 1: the chain was rewritten, so the recomputed root differs.
		Entries: rewriteActors(t, 6), Anchor: anchor, InstallID: treeTestInstall,
		WantProblem: "the history under the anchor was rewritten",
	}, { // Test 2: no install identity, so the tree cannot be recomputed at all.
		Entries: chain, Anchor: anchor, InstallID: "",
		WantProblem: "identity is unavailable",
	}, { // Test 3: an undefined shape is refused, never checked as either coordinate space.
		Entries: chain, InstallID: treeTestInstall,
		Anchor: &Anchor{ID: "anc_odd", Type: AnchorHTTPS, Shape: "spiral", Seq: 6,
			Link: anchor.Link, At: time.Now()},
		WantProblem: "unknown coordinate shape",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ok, results := CheckAnchors(test.Entries, []*Anchor{test.Anchor}, test.InstallID)
			if ok {
				t.Fatal("CheckAnchors() passed a chain that cannot satisfy this anchor")
			}
			if len(results) != 1 || results[0].Reached {
				t.Fatalf("results = %+v, want one unreached anchor", results)
			}
			if got := results[0].Problem; !strings.Contains(got, test.WantProblem) {
				t.Errorf("problem = %q, want it to mention %q", got, test.WantProblem)
			}
		})
	}
}

// TestCheckAnchorsMixedShapes proves linear and tree anchors are checked in their own coordinate
// spaces side by side: both hold on the intact chain, and truncation fails both with their own
// wording.
func TestCheckAnchorsMixedShapes(t *testing.T) {
	t.Parallel()
	chain := buildChain(t, 5)
	linear := &Anchor{
		ID: "anc_lin", Type: AnchorRFC3161, Shape: AnchorShapeLinear,
		Seq: chain[4].Seq, Link: chain[4].Hash, At: time.Now(),
	}
	tree := treeAnchorAt(t, chain, 5)

	if ok, results := CheckAnchors(chain, []*Anchor{linear, tree}, treeTestInstall); !ok {
		t.Fatalf("the intact chain fails its own anchors: %+v", results)
	}
	ok, results := CheckAnchors(chain[:3], []*Anchor{linear, tree}, treeTestInstall)
	if ok {
		t.Fatal("a truncated chain satisfied anchors over its lost tail")
	}
	for _, res := range results {
		if res.Reached {
			t.Errorf("anchor %s reached over a truncated chain", res.Anchor.ID)
		}
	}
}

// rewriteActors returns a freshly linked chain of n entries whose content differs from buildChain.
func rewriteActors(t *testing.T, n int) []*Entry {
	t.Helper()
	chain := buildChain(t, n)
	var prev string
	for _, e := range chain {
		e.Actor = "mallory"
		e.PrevHash = prev
		e.Hash = EntryHash(e)
		prev = e.Hash
	}
	return chain
}
