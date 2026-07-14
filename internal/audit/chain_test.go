package audit_test

import (
	"testing"
	"time"

	"github.com/dcadolph/railwarden/internal/audit"
)

// buildChain returns a valid chain of n entries linked with audit.Link.
func buildChain(n int) []*audit.Entry {
	base := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	var out []*audit.Entry
	var prev *audit.Entry
	for i := 0; i < n; i++ {
		e := &audit.Entry{
			ID: audit.NewID(), At: base.Add(time.Duration(i) * time.Second),
			Actor: "root", Method: "POST", Path: "/runs",
		}
		audit.Link(prev, e)
		out = append(out, e)
		prev = e
	}
	return out
}

// TestLinkGenesis confirms the first entry starts the chain at sequence one with no predecessor.
func TestLinkGenesis(t *testing.T) {
	t.Parallel()
	e := &audit.Entry{ID: "aud_1", At: time.Unix(0, 0).UTC(), Actor: "root", Method: "POST", Path: "/runs"}
	audit.Link(nil, e)
	if e.Seq != 1 {
		t.Errorf("Seq = %d, want 1", e.Seq)
	}
	if e.PrevHash != "" {
		t.Errorf("PrevHash = %q, want empty", e.PrevHash)
	}
	if e.Hash != audit.EntryHash(e) {
		t.Errorf("Hash = %q, want the entry hash %q", e.Hash, audit.EntryHash(e))
	}
}

// TestEntryHashDependsOnContent confirms the hash changes when a field changes.
func TestEntryHashDependsOnContent(t *testing.T) {
	t.Parallel()
	e := &audit.Entry{ID: "aud_1", At: time.Unix(0, 0).UTC(), Actor: "root", Method: "POST", Path: "/runs", Seq: 1}
	before := audit.EntryHash(e)
	e.Actor = "mallory"
	if after := audit.EntryHash(e); after == before {
		t.Errorf("hash unchanged after editing actor: %q", after)
	}
}

// TestVerify covers an intact chain and the tamper cases: an edited field, a rewritten hash, a
// broken link, a deleted entry, and a blanked hash.
func TestVerify(t *testing.T) {
	t.Parallel()

	// Test 0: An intact chain verifies.
	if ok, at := audit.Verify(buildChain(3)); !ok {
		t.Errorf("intact chain: Verify broke at %d", at)
	}

	// Test 1: Editing a field without rehashing is caught at that entry.
	tampered := buildChain(3)
	tampered[1].Actor = "mallory"
	if ok, at := audit.Verify(tampered); ok || at != 2 {
		t.Errorf("edited field: ok=%v at=%d, want false at 2", ok, at)
	}

	// Test 2: Rewriting an entry's own hash is caught at that entry.
	rehashed := buildChain(3)
	rehashed[0].Hash = "deadbeef"
	if ok, at := audit.Verify(rehashed); ok || at != 1 {
		t.Errorf("rewritten hash: ok=%v at=%d, want false at 1", ok, at)
	}

	// Test 3: Breaking the link to the previous entry is caught.
	relinked := buildChain(3)
	relinked[2].PrevHash = "deadbeef"
	if ok, at := audit.Verify(relinked); ok || at != 3 {
		t.Errorf("broken link: ok=%v at=%d, want false at 3", ok, at)
	}

	// Test 4: Deleting a middle entry leaves a sequence gap that is caught.
	full := buildChain(3)
	gapped := []*audit.Entry{full[0], full[2]}
	if ok, at := audit.Verify(gapped); ok || at != 2 {
		t.Errorf("deleted entry: ok=%v at=%d, want false at 2", ok, at)
	}

	// Test 5: An entry with no hash breaks the chain, so a blanked entry cannot hide from verification.
	blanked := buildChain(3)
	blanked[1].Hash = ""
	if ok, at := audit.Verify(blanked); ok || at != 2 {
		t.Errorf("blanked hash: ok=%v at=%d, want false at 2", ok, at)
	}
}
