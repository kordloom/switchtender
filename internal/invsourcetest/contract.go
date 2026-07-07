// Package invsourcetest provides a shared behavior contract for invsource.Store implementations
// so the in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package invsourcetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dcadolph/yardmaster/internal/invsource"
)

// Contract runs the invsource.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() invsource.Store) {
	t.Helper()
	t.Run("lifecycle", func(t *testing.T) { testLifecycle(t, newStore()) })
	t.Run("list ordered", func(t *testing.T) { testList(t, newStore()) })
}

// testLifecycle verifies a source round trips with its sync fields and deletes.
func testLifecycle(t *testing.T, store invsource.Store) {
	ctx := context.Background()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	synced := created.Add(time.Hour)
	s := &invsource.Source{
		ID: "src_1", Name: "aws", Source: "aws_ec2.yml", CredentialID: "cred_9",
		ProjectID: "proj_2", InventoryID: "inv_3", SyncedAt: &synced,
		LastError: "", CreatedAt: created,
	}
	if err := store.Save(ctx, s); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Get(ctx, "src_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Name != "aws" || got.Source != "aws_ec2.yml" || got.InventoryID != "inv_3" ||
		got.SyncedAt == nil || !got.SyncedAt.Equal(synced) {
		t.Errorf("Get() = %+v, want the saved source", got)
	}

	if _, err := store.Get(ctx, "ghost"); !errors.Is(err, invsource.ErrNotFound) {
		t.Errorf("Get(ghost) error = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, "src_1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(ctx, "src_1"); !errors.Is(err, invsource.ErrNotFound) {
		t.Errorf("Delete(gone) error = %v, want ErrNotFound", err)
	}
}

// testList verifies sources come back oldest first.
func testList(t *testing.T, store invsource.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"src_b", "src_a"} {
		if err := store.Save(ctx, &invsource.Source{
			ID: id, Name: id, Source: "s", InventoryID: "inv_" + id,
			CreatedAt: base.Add(time.Duration(1-i) * time.Hour),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 2 || list[0].ID != "src_a" || list[1].ID != "src_b" {
		t.Errorf("List() order = %+v, want src_a then src_b", list)
	}
}
