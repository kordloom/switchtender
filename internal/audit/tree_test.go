package audit_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// treeChain returns a linked chain of n entries, standing in for an install's audit log.
func treeChain(t *testing.T, n int) []*audit.Entry {
	t.Helper()
	at := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	var chain []*audit.Entry
	var prev *audit.Entry
	for i := 0; i < n; i++ {
		e := &audit.Entry{
			ID: audit.NewID(), At: at.Add(time.Duration(i) * time.Minute),
			Actor: "alice", Method: "POST", Path: "/v1/runs/" + string(rune('a'+i)),
		}
		audit.Link(prev, e)
		chain = append(chain, e)
		prev = e
	}
	return chain
}

// treeIdentity returns a producer identity for a test.
func treeIdentity(t *testing.T) audit.Identity {
	t.Helper()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	return id
}

// TestTreeBundleDisclosesOnlyItsOwnEntries is the point of the tree profile: a bundle proves two
// entries belong to a ten entry log while carrying nothing about the other eight. The linear profile
// cannot do this, because proving an entry means shipping the run of entries that reaches it.
func TestTreeBundleDisclosesOnlyItsOwnEntries(t *testing.T) {
	chain := treeChain(t, 10)
	id := treeIdentity(t)

	doc, err := audit.BuildTreeBundle(chain, map[int64]bool{3: true, 7: true}, id, "v-test",
		audit.BundleSubject{Type: "run", ID: "run_demo"}, time.Now())
	if err != nil {
		t.Fatalf("BuildTreeBundle() error = %v", err)
	}
	if len(doc.Claims) != 2 {
		t.Fatalf("claims = %d, want only the two disclosed", len(doc.Claims))
	}
	if doc.Chain.Head.Seq != 10 {
		t.Errorf("head seq = %d, want the tree size 10", doc.Chain.Head.Seq)
	}
	for i, c := range doc.Claims {
		if c.Inclusion == nil {
			t.Errorf("claim %d carries no inclusion proof", i)
		}
		if c.Chain.Prev != "" {
			t.Errorf("claim %d carries a previous link, which a tree has no place for", i)
		}
	}

	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}
	// The undisclosed entries must not appear anywhere in the document, in any form. Their paths are
	// the readable part of an entry, so finding one would mean the receipt leaked a neighbor.
	body := string(signed)
	for _, e := range chain {
		if e.Seq == 3 || e.Seq == 7 {
			continue
		}
		if strings.Contains(body, e.Path) {
			t.Errorf("the receipt leaks undisclosed entry %d at path %s", e.Seq, e.Path)
		}
	}
}

// TestTreeBundleVerifiesInLoomSeal is the integration that matters: a bundle this product emits is
// checked by the format's own verifier, not by a second copy of this product's logic. It runs the
// loomseal verifier over the signed document and requires a clean verdict.
func TestTreeBundleVerifiesInLoomSeal(t *testing.T) {
	chain := treeChain(t, 10)
	id := treeIdentity(t)

	doc, err := audit.BuildTreeBundle(chain, map[int64]bool{2: true, 5: true, 9: true}, id, "v-test",
		audit.BundleSubject{Type: "run", ID: "run_demo"}, time.Now())
	if err != nil {
		t.Fatalf("BuildTreeBundle() error = %v", err)
	}
	// Prove the log only ever appended, from a size an earlier reader would have seen.
	if err := doc.AttachConsistency(chain, 6, id); err != nil {
		t.Fatalf("AttachConsistency() error = %v", err)
	}
	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}

	report := verifyWithLoomSeal(t, signed)
	if !report.OK {
		t.Fatalf("loomseal refused a bundle this product emitted: %v", report.Problems)
	}
	if report.TreeSize != 10 {
		t.Errorf("reported tree size = %d, want 10", report.TreeSize)
	}
	if report.InclusionProofs != 3 {
		t.Errorf("inclusion proofs verified = %d, want 3", report.InclusionProofs)
	}
	if !report.ConsistencyOK || report.ConsistencyFrom != 6 {
		t.Errorf("consistency ok=%v from=%d, want true from 6", report.ConsistencyOK,
			report.ConsistencyFrom)
	}
}

// TestTreeBundleRefusesABrokenChain checks a receipt is never drawn from a chain that does not hold
// together. A tree built over a rewritten chain would verify against its own root, which is exactly
// the reassuring lie this refuses to produce.
func TestTreeBundleRefusesABrokenChain(t *testing.T) {
	chain := treeChain(t, 5)
	id := treeIdentity(t)
	chain[2].Path = "/v1/rewritten"

	if _, err := audit.BuildTreeBundle(chain, map[int64]bool{1: true}, id, "v-test",
		audit.BundleSubject{Type: "run", ID: "run_demo"}, time.Now()); err == nil {
		t.Error("a receipt was built from a chain that does not verify")
	}
}

// loomsealReport is the part of the format verifier's report these tests assert on.
type loomsealReport struct {
	// OK reports whether every check passed.
	OK bool `json:"ok"`
	// Problems lists why it did not.
	Problems []string `json:"problems"`
	// TreeSize is the size of the log the head attests.
	TreeSize int64 `json:"tree_size"`
	// InclusionProofs is how many audit paths folded to the root.
	InclusionProofs int `json:"inclusion_proofs"`
	// ConsistencyFrom and ConsistencyOK report an append-only proof.
	ConsistencyFrom int64 `json:"consistency_from"`
	ConsistencyOK   bool  `json:"consistency_ok"`
}

// verifyWithLoomSeal runs the format's own verifier over a bundle, so this product's output is
// checked by the reference implementation rather than by itself.
func verifyWithLoomSeal(t *testing.T, signed []byte) loomsealReport {
	t.Helper()
	out := runLoomSealVerify(t, signed)
	var rep loomsealReport
	if err := json.Unmarshal(out, &rep); err != nil {
		t.Fatalf("loomseal report is not JSON: %v\n%s", err, out)
	}
	return rep
}
