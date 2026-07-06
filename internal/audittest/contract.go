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
