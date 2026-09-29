// Package audittest provides a shared behavior contract for audit.Store implementations so the
// in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package audittest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// Contract runs the audit.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() audit.Store) {
	t.Helper()
	t.Run("append and list", func(t *testing.T) { testAppendList(t, newStore()) })
	t.Run("chain verifies", func(t *testing.T) { testChain(t, newStore()) })
	t.Run("anchors round trip and scope", func(t *testing.T) { testAnchors(t, newStore()) })
	t.Run("concurrent appends do not fork", func(t *testing.T) { testConcurrentAppend(t, newStore()) })
	t.Run("empty list is non-nil", func(t *testing.T) {
		got, err := newStore().List(context.Background(), 10)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if got == nil {
			t.Error("List() on an empty store = nil, want a non-nil empty slice")
		}
	})
}

// testConcurrentAppend fires many appends at once and checks the chain stays a single intact line:
// contiguous unique sequences and a passing Verify. A store that reads its head and inserts without
// serializing forks here, showing up as a duplicate sequence or a broken chain.
func testConcurrentAppend(t *testing.T, store audit.Store) {
	ctx := context.Background()
	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := store.Append(ctx, &audit.Entry{
				ID: audit.NewID(), At: time.Unix(int64(i), 0).UTC(),
				Actor: "root", Method: "POST", Path: "/runs",
			}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Append() error = %v", err)
	}

	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != n {
		t.Fatalf("Chain() len = %d, want %d", len(chain), n)
	}
	seen := make(map[int64]bool, n)
	for i, e := range chain {
		if e.Seq != int64(i+1) {
			t.Errorf("entry %d seq = %d, want %d", i, e.Seq, i+1)
		}
		if seen[e.Seq] {
			t.Errorf("duplicate seq %d: the chain forked", e.Seq)
		}
		seen[e.Seq] = true
	}
	if ok, at := audit.Verify(chain); !ok {
		t.Errorf("Verify() reported a break at %d after concurrent appends", at)
	}
}

// testChain verifies that appended entries form an intact hash chain with contiguous sequences that
// survives a store round-trip.
func testChain(t *testing.T, store audit.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	for i, path := range []string{"/runs", "/projects", "/users", "/schedules"} {
		if err := store.Append(ctx, &audit.Entry{
			ID: audit.NewID(), At: base.Add(time.Duration(i) * time.Minute),
			Actor: "root", Method: "POST", Path: path,
		}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}

	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != 4 {
		t.Fatalf("Chain() len = %d, want 4", len(chain))
	}
	for i, e := range chain {
		if e.Seq != int64(i+1) {
			t.Errorf("entry %d seq = %d, want %d", i, e.Seq, i+1)
		}
		if e.Hash == "" {
			t.Errorf("entry %d has no hash", i)
		}
	}
	if chain[0].PrevHash != "" {
		t.Errorf("first entry prev_hash = %q, want empty", chain[0].PrevHash)
	}
	if chain[1].PrevHash != chain[0].Hash {
		t.Errorf("second entry prev_hash = %q, want the first hash %q", chain[1].PrevHash, chain[0].Hash)
	}
	if ok, at := audit.Verify(chain); !ok {
		t.Errorf("Verify() reported a break at %d, want an intact chain", at)
	}
}

// testAppendList verifies entries come back newest first with the limit honored.
func testAppendList(t *testing.T, store audit.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	for i, path := range []string{"/runs", "/projects", "/users"} {
		if err := store.Append(ctx, &audit.Entry{
			ID: audit.NewID(), At: base.Add(time.Duration(i) * time.Minute),
			Actor: "root", Method: "POST", Path: path,
		}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}

	all, err := store.List(ctx, 10)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(all) != 3 || all[0].Path != "/users" || all[2].Path != "/runs" {
		t.Errorf("List() = %+v, want newest first users..runs", all)
	}
	if all[0].Actor != "root" || all[0].Method != "POST" {
		t.Errorf("entry = %+v, want actor root method POST", all[0])
	}

	one, err := store.List(ctx, 1)
	if err != nil {
		t.Fatalf("List(1) error = %v", err)
	}
	if len(one) != 1 || one[0].Path != "/users" {
		t.Errorf("List(1) = %+v, want just the newest", one)
	}
}

// testAnchors verifies that anchors persist, come back oldest first, and scope to a sequence.
//
// An anchor fixes a chain link somewhere the operator cannot rewrite alone, which is what turns "the
// chain has not been altered" into "the chain has also not been shortened". That only holds if the
// anchor itself survives, so it is stored beside the entries rather than left in a log line, and a
// bundle asks for the anchors covering the range it carries rather than all of them.
//
// Identifiers are generated rather than fixed because a contract store may be shared across cases,
// so the assertions are about the anchors this case saved rather than about absolute counts.
func testAnchors(t *testing.T, store audit.Store) {
	t.Helper()
	ctx := context.Background()
	anchors, ok := store.(audit.AnchorStore)
	if !ok {
		t.Fatalf("%T does not persist anchors, so a chain it holds cannot be fixed in time", store)
	}
	before, err := anchors.Anchors(ctx, 0)
	if err != nil {
		t.Fatalf("Anchors() error = %v", err)
	}

	at := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	stamped := &audit.Anchor{
		ID: audit.NewAnchorID(), Type: audit.AnchorRFC3161, Seq: 5, Link: "aa", At: at,
		Ref: "https://freetsa.org/tsr", Proof: "MIIBase64Token",
	}
	committed := &audit.Anchor{
		ID: audit.NewAnchorID(), Type: audit.AnchorGit, Seq: 20, Link: "bb", At: at.Add(time.Hour),
		Ref: "https://github.com/acme/anchors/commit/deadbeef",
	}
	for _, a := range []*audit.Anchor{stamped, committed} {
		if err := anchors.SaveAnchor(ctx, a); err != nil {
			t.Fatalf("SaveAnchor(%s) error = %v", a.ID, err)
		}
	}

	all, err := anchors.Anchors(ctx, 0)
	if err != nil {
		t.Fatalf("Anchors(0) error = %v", err)
	}
	if len(all) != len(before)+2 {
		t.Fatalf("Anchors(0) returned %d, want %d", len(all), len(before)+2)
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].Seq > all[i].Seq {
			t.Errorf("anchors came back out of order at index %d, want oldest first", i)
		}
	}

	byID := make(map[string]*audit.Anchor, len(all))
	for _, a := range all {
		byID[a.ID] = a
	}
	got, ok := byID[stamped.ID]
	if !ok {
		t.Fatal("the saved rfc3161 anchor did not come back")
	}
	// The embedded proof is the whole value of an rfc3161 anchor. Losing it in storage would leave a
	// record that looks anchored and proves nothing.
	if got.Proof != stamped.Proof {
		t.Errorf("proof = %q, want it stored verbatim", got.Proof)
	}
	if got.Ref != stamped.Ref || got.Type != audit.AnchorRFC3161 || got.Link != "aa" {
		t.Errorf("anchor = %+v, want its type, link, and reference preserved", got)
	}
	if !got.At.Equal(at) {
		t.Errorf("anchor time = %s, want %s", got.At, at)
	}

	// A bundle covering the first ten entries must not carry an anchor for entry twenty: a verifier
	// rejects a bundle whose anchor names a link it does not hold.
	scoped, err := anchors.Anchors(ctx, 10)
	if err != nil {
		t.Fatalf("Anchors(10) error = %v", err)
	}
	found := false
	for _, a := range scoped {
		if a.Seq > 10 {
			t.Errorf("Anchors(10) returned an anchor at seq %d, which names a link a bundle of the "+
				"first ten entries does not hold", a.Seq)
		}
		if a.ID == stamped.ID {
			found = true
		}
		if a.ID == committed.ID {
			t.Error("Anchors(10) returned the anchor at seq 20")
		}
	}
	if !found {
		t.Error("Anchors(10) dropped the anchor at seq 5, which is inside the range")
	}
}
