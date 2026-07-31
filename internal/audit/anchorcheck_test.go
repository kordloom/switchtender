package audit

import (
	"fmt"
	"testing"
	"time"
)

// buildChain returns a valid chain of n entries, each linked to the one before it.
func buildChain(t *testing.T, n int) []*Entry {
	t.Helper()
	at := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	entries := make([]*Entry, 0, n)
	var prev string
	for i := 1; i <= n; i++ {
		e := &Entry{
			ID: fmt.Sprintf("aud_%03d", i), Seq: int64(i), At: at.Add(time.Duration(i) * time.Second),
			Actor: "alice", Method: "POST", Path: "/v1/runs", PrevHash: prev,
		}
		e.Hash = EntryHash(e)
		prev = e.Hash
		entries = append(entries, e)
	}
	if ok, at := Verify(entries); !ok {
		t.Fatalf("built chain does not verify at %d", at)
	}
	return entries
}

// TestCheckAnchorsCatchesATruncatedTail is the case anchoring exists for.
//
// A hash chain shows nothing in it was altered. It cannot show nothing was removed from the end,
// because a prefix of a valid chain is itself a valid chain: drop the last entries and what remains
// has the same genesis, an unbroken run of links, and verifies perfectly. Front truncation is caught
// because the chain must start at sequence one with no predecessor. Tail truncation is caught by
// nothing except an anchor, and nothing was consulting the anchors, so a shortened chain was
// reported healthy while the evidence that disproved it sat in the same database.
func TestCheckAnchorsCatchesATruncatedTail(t *testing.T) {
	t.Parallel()
	full := buildChain(t, 10)
	head := full[len(full)-1]
	anchor := &Anchor{
		ID: "anc_head", Type: AnchorRFC3161, Seq: head.Seq, Link: head.Hash,
		At: time.Now(), Ref: "https://freetsa.org/tsr",
	}

	// The untouched chain satisfies its anchor.
	if ok, results := CheckAnchors(full, []*Anchor{anchor}); !ok {
		t.Fatalf("the full chain fails its own anchor: %+v", results)
	}

	// Four entries removed from the end. The chain still hash-verifies, which is the whole problem.
	truncated := full[:6]
	if ok, brokeAt := Verify(truncated); !ok {
		t.Fatalf("a truncated chain should still verify by hash alone, broke at %d", brokeAt)
	}
	ok, results := CheckAnchors(truncated, []*Anchor{anchor})
	if ok {
		t.Fatal("a chain missing four entries satisfied an anchor taken over its head, so " +
			"truncation is invisible to every verification path")
	}
	if len(results) != 1 || results[0].Reached {
		t.Fatalf("results = %+v, want one unreached anchor", results)
	}
	t.Logf("reported: %s", results[0].Problem)
}

// TestCheckAnchorsCatchesRewrittenHistory covers the other thing an anchor proves: the chain reaches
// the anchored position but no longer holds the link that was there.
func TestCheckAnchorsCatchesRewrittenHistory(t *testing.T) {
	t.Parallel()
	original := buildChain(t, 5)
	anchor := &Anchor{
		ID: "anc_mid", Type: AnchorRFC3161, Seq: 3, Link: original[2].Hash, At: time.Now(),
	}

	// A chain of the same length whose entries differ, rebuilt so it verifies cleanly.
	rewritten := buildChain(t, 5)
	for _, e := range rewritten {
		e.Actor = "mallory"
	}
	var prev string
	for _, e := range rewritten {
		e.PrevHash = prev
		e.Hash = EntryHash(e)
		prev = e.Hash
	}
	if ok, at := Verify(rewritten); !ok {
		t.Fatalf("the rewritten chain should verify by hash alone, broke at %d", at)
	}

	ok, results := CheckAnchors(rewritten, []*Anchor{anchor})
	if ok {
		t.Fatal("a rewritten history satisfied an anchor taken over the original")
	}
	t.Logf("reported: %s", results[0].Problem)
}

// TestCheckAnchorsPassesAnUnanchoredChain checks that an install which has never anchored gets
// nothing from this rather than a false assurance, and is not failed for it either.
func TestCheckAnchorsPassesAnUnanchoredChain(t *testing.T) {
	t.Parallel()
	ok, results := CheckAnchors(buildChain(t, 4), nil)
	if !ok {
		t.Error("a chain with no anchors over it was reported as failing one")
	}
	if len(results) != 0 {
		t.Errorf("results = %+v, want none", results)
	}
}
