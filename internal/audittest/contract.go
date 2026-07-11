// Package audittest provides a shared behavior contract for audit.Store implementations so the
// in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package audittest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dcadolph/yardmaster/internal/audit"
)

// Contract runs the audit.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() audit.Store) {
	t.Helper()
	t.Run("append and list", func(t *testing.T) { testAppendList(t, newStore()) })
	t.Run("chain verifies", func(t *testing.T) { testChain(t, newStore()) })
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
