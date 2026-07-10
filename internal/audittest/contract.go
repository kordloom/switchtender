// Package audittest provides a shared behavior contract for audit.Store implementations so the
// in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package audittest

import (
	"context"
	"testing"
	"time"

	"github.com/dcadolph/yardmaster/internal/audit"
)

// Contract runs the audit.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() audit.Store) {
	t.Helper()
	t.Run("append and list", func(t *testing.T) { testAppendList(t, newStore()) })
	t.Run("chain verifies", func(t *testing.T) { testChain(t, newStore()) })
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
