package audit_test

import (
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// TestVerifyBundleAcceptsRootAnchoredSparseReceipt proves an honest sparse receipt anchored at its
// own root verifies. The anchor names the tree head's coordinates, which the chain check has
// already validated by folding every disclosed claim to that root, so refusing it refused the
// output of receipt --sparse on any anchored install.
func TestVerifyBundleAcceptsRootAnchoredSparseReceipt(t *testing.T) {
	chain := treeChain(t, 6)
	id := treeIdentity(t)
	size, root, err := audit.TreeHead(chain, id.InstallID)
	if err != nil {
		t.Fatalf("TreeHead() error = %v", err)
	}

	doc, err := audit.BuildTreeBundle(chain, map[int64]bool{3: true}, id, "v-test",
		audit.BundleSubject{Type: "run", ID: "run_demo"}, time.Now())
	if err != nil {
		t.Fatalf("BuildTreeBundle() error = %v", err)
	}
	kept := doc.AttachAnchors([]*audit.Anchor{{
		ID: "anc_root", Type: audit.AnchorHTTPS, Shape: audit.AnchorShapeTree,
		Seq: size, Link: root, At: time.Now(), Ref: "https://example.com/head",
	}})
	if kept != 1 {
		t.Fatalf("AttachAnchors() kept %d, want the root anchor attached", kept)
	}
	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}

	rep, err := audit.VerifyBundle(signed, "")
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if !rep.AnchorsOK {
		t.Error("VerifyBundle() refused the anchor over the receipt's own root")
	}
	if !rep.OK() {
		t.Errorf("VerifyBundle() verdict = %+v, want the root-anchored receipt accepted", rep)
	}
	if rep.AnchorCount != 1 {
		t.Errorf("AnchorCount = %d, want 1", rep.AnchorCount)
	}
}

// TestVerifyBundleAcceptsConsistencyRootAnchor proves a receipt drawn after the log grew still
// verifies when its anchor fixes the earlier root a consistency proof starts from. That pairing is
// what makes truncation refutable, so it is the receipt shape an anchored install actually emits.
func TestVerifyBundleAcceptsConsistencyRootAnchor(t *testing.T) {
	id := treeIdentity(t)
	anchoredSize := int64(4)
	early := treeChain(t, int(anchoredSize))
	_, earlyRoot, err := audit.TreeHead(early, id.InstallID)
	if err != nil {
		t.Fatalf("TreeHead(early) error = %v", err)
	}

	grown := treeChain(t, 7)
	doc, err := audit.BuildTreeBundle(grown, map[int64]bool{2: true}, id, "v-test",
		audit.BundleSubject{Type: "run", ID: "run_demo"}, time.Now())
	if err != nil {
		t.Fatalf("BuildTreeBundle() error = %v", err)
	}
	if err := doc.AttachConsistency(grown, anchoredSize, id); err != nil {
		t.Fatalf("AttachConsistency() error = %v", err)
	}
	kept := doc.AttachAnchors([]*audit.Anchor{{
		ID: "anc_early", Type: audit.AnchorHTTPS, Shape: audit.AnchorShapeTree,
		Seq: anchoredSize, Link: earlyRoot, At: time.Now(), Ref: "https://example.com/head",
	}})
	if kept != 1 {
		t.Fatalf("AttachAnchors() kept %d, want the consistency-root anchor attached", kept)
	}
	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}

	rep, err := audit.VerifyBundle(signed, "")
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if !rep.AnchorsOK || !rep.OK() {
		t.Errorf("VerifyBundle() verdict = %+v, want the consistency-root anchor accepted", rep)
	}
}

// TestVerifyBundleRejectsForgedConsistency proves the consistency proof is verified, not merely
// carried. Admitting the from-root as an anchor coordinate is only sound because a proof that does
// not fold from it to the head fails the chain check.
func TestVerifyBundleRejectsForgedConsistency(t *testing.T) {
	id := treeIdentity(t)
	grown := treeChain(t, 7)
	doc, err := audit.BuildTreeBundle(grown, map[int64]bool{2: true}, id, "v-test",
		audit.BundleSubject{Type: "run", ID: "run_demo"}, time.Now())
	if err != nil {
		t.Fatalf("BuildTreeBundle() error = %v", err)
	}
	if err := doc.AttachConsistency(grown, 4, id); err != nil {
		t.Fatalf("AttachConsistency() error = %v", err)
	}
	// The producer swaps in a from-root the log never had, keeping the proof path. An anchor over
	// that root would then read as independent evidence for a fabricated history.
	doc.Chain.Consistency.FromRoot = "00" + doc.Chain.Consistency.FromRoot[2:]
	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}

	rep, err := audit.VerifyBundle(signed, "")
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if rep.ChainOK {
		t.Error("VerifyBundle() accepted a consistency proof from a root the log never had")
	}
	if rep.OK() {
		t.Error("VerifyBundle() verdict OK over a forged consistency root")
	}
}
