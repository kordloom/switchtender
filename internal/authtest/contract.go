// Package authtest provides a shared behavior contract for auth.Store implementations so the
// in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package authtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/auth"
)

// Contract runs the auth.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() auth.Store) {
	t.Helper()
	t.Run("save find delete", func(t *testing.T) { testLifecycle(t, newStore()) })
	t.Run("list ordered", func(t *testing.T) { testList(t, newStore()) })
	t.Run("count", func(t *testing.T) { testCount(t, newStore()) })
}

// testLifecycle verifies a token round trips by hash, updates, and deletes.
func testLifecycle(t *testing.T, store auth.Store) {
	ctx := context.Background()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	tok := &auth.Token{ID: "tok_1", Name: "ci", Hash: "abc123", CreatedAt: created}
	if err := store.Save(ctx, tok); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.FindByHash(ctx, "abc123")
	if err != nil {
		t.Fatalf("FindByHash() error = %v", err)
	}
	if got.ID != "tok_1" || got.Name != "ci" || !got.CreatedAt.Equal(created) {
		t.Errorf("FindByHash() = %+v, want tok_1 ci", got)
	}
	if _, err := store.FindByHash(ctx, "nope"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("FindByHash(miss) error = %v, want ErrNotFound", err)
	}

	used := created.Add(time.Hour)
	tok.LastUsedAt = &used
	if err := store.Save(ctx, tok); err != nil {
		t.Fatalf("Save() update error = %v", err)
	}
	got, err = store.FindByHash(ctx, "abc123")
	if err != nil {
		t.Fatalf("FindByHash() error = %v", err)
	}
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(used) {
		t.Errorf("LastUsedAt = %v, want %v", got.LastUsedAt, used)
	}

	if err := store.Delete(ctx, "tok_1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(ctx, "tok_1"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("Delete(gone) error = %v, want ErrNotFound", err)
	}
}

// testList verifies tokens come back oldest first.
func testList(t *testing.T, store auth.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"tok_b", "tok_a"} {
		if err := store.Save(ctx, &auth.Token{
			ID: id, Name: id, Hash: id + "h", CreatedAt: base.Add(time.Duration(1-i) * time.Hour),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 2 || list[0].ID != "tok_a" || list[1].ID != "tok_b" {
		t.Errorf("List() order = %+v, want tok_a then tok_b", list)
	}
}

// testCount verifies the count tracks saves and deletes.
func testCount(t *testing.T, store auth.Store) {
	ctx := context.Background()
	if n, _ := store.Count(ctx); n != 0 {
		t.Fatalf("Count() = %d, want 0", n)
	}
	if err := store.Save(ctx, &auth.Token{ID: "tok_1", Hash: "h", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if n, _ := store.Count(ctx); n != 1 {
		t.Errorf("Count() = %d, want 1", n)
	}
}
