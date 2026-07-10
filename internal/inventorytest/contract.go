// Package inventorytest provides a shared behavior contract for inventory.Store implementations
// so the in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package inventorytest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/dcadolph/yardmaster/internal/inventory"
)

// Contract runs the inventory.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() inventory.Store) {
	t.Helper()
	t.Run("lifecycle", func(t *testing.T) { testLifecycle(t, newStore()) })
	t.Run("list ordered", func(t *testing.T) { testList(t, newStore()) })
	t.Run("update", func(t *testing.T) { testUpdate(t, newStore()) })
}

// testUpdate verifies an update changes name and content, preserves the creation time, and reports
// ErrNotFound for an unknown id.
func testUpdate(t *testing.T, store inventory.Store) {
	ctx := context.Background()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Save(ctx, &inventory.Inventory{
		ID: "inv_1", Name: "old", Content: "a", CreatedAt: created,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if err := store.Update(ctx, &inventory.Inventory{
		ID: "inv_1", Name: "new", Content: "b", CredentialIDs: []string{"cred_x"},
		ContentSource: "command", ContentConfig: "sealed-cmd",
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	got, err := store.Get(ctx, "inv_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Name != "new" || got.Content != "b" {
		t.Errorf("Get() = %+v, want updated name and content", got)
	}
	if !slices.Equal(got.CredentialIDs, []string{"cred_x"}) {
		t.Errorf("CredentialIDs after update = %v, want [cred_x]", got.CredentialIDs)
	}
	if got.ContentSource != "command" || got.ContentConfig != "sealed-cmd" {
		t.Errorf("content source after update = %q/%q, want command/sealed-cmd", got.ContentSource, got.ContentConfig)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want preserved %v", got.CreatedAt, created)
	}

	if err := store.Update(ctx, &inventory.Inventory{ID: "ghost", Name: "x", Content: "y"}); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("Update(ghost) error = %v, want ErrNotFound", err)
	}
}

// testLifecycle verifies an inventory round trips with its content and deletes.
func testLifecycle(t *testing.T, store inventory.Store) {
	ctx := context.Background()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	i := &inventory.Inventory{
		ID: "inv_1", Name: "fleet", Content: "[web]\nweb01\n",
		CredentialIDs: []string{"cred_a", "cred_b"},
		ContentSource: "vault", ContentConfig: "sealed-config-blob", CreatedAt: created,
	}
	if err := store.Save(ctx, i); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Get(ctx, "inv_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Name != "fleet" || got.Content != "[web]\nweb01\n" || !got.CreatedAt.Equal(created) {
		t.Errorf("Get() = %+v, want the saved inventory", got)
	}
	if !slices.Equal(got.CredentialIDs, []string{"cred_a", "cred_b"}) {
		t.Errorf("CredentialIDs = %v, want [cred_a cred_b]", got.CredentialIDs)
	}
	if got.ContentSource != "vault" || got.ContentConfig != "sealed-config-blob" {
		t.Errorf("content source = %q/%q, want vault/sealed-config-blob", got.ContentSource, got.ContentConfig)
	}

	if _, err := store.Get(ctx, "ghost"); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("Get(ghost) error = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, "inv_1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(ctx, "inv_1"); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("Delete(gone) error = %v, want ErrNotFound", err)
	}
}

// testList verifies inventories come back oldest first.
func testList(t *testing.T, store inventory.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"inv_b", "inv_a"} {
		if err := store.Save(ctx, &inventory.Inventory{
			ID: id, Name: id, Content: "x", CreatedAt: base.Add(time.Duration(1-i) * time.Hour),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 2 || list[0].ID != "inv_a" || list[1].ID != "inv_b" {
		t.Errorf("List() order = %+v, want inv_a then inv_b", list)
	}
}
