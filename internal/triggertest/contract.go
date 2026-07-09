// Package triggertest provides a shared behavior contract for trigger.Store implementations so
// the in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package triggertest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dcadolph/yardmaster/internal/trigger"
)

// Contract runs the trigger.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() trigger.Store) {
	t.Helper()
	t.Run("lifecycle", func(t *testing.T) { testLifecycle(t, newStore()) })
	t.Run("find by token hash", func(t *testing.T) { testFindByHash(t, newStore()) })
	t.Run("list ordered", func(t *testing.T) { testList(t, newStore()) })
	t.Run("signing secret", func(t *testing.T) { testSigning(t, newStore()) })
}

// testSigning verifies the sealed signing secret and enforcement flag round trip.
func testSigning(t *testing.T, store trigger.Store) {
	ctx := context.Background()
	tg := &trigger.Trigger{
		ID: "trg_sig", Name: "signed", TemplateID: "tpl_1", TokenHash: "th",
		SigningSecret: "sealed-secret", RequireSignature: true, CreatedAt: time.Now(),
	}
	if err := store.Save(ctx, tg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Get(ctx, "trg_sig")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.SigningSecret != "sealed-secret" || !got.RequireSignature {
		t.Errorf("Get() = %+v, want sealed secret and require signature set", got)
	}
}

// testLifecycle verifies a trigger round trips, updates, and deletes.
func testLifecycle(t *testing.T, store trigger.Store) {
	ctx := context.Background()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	fired := created.Add(time.Hour)
	tg := &trigger.Trigger{
		ID: "trg_1", Name: "deploy", TemplateID: "tpl_9", TokenHash: "abc",
		LastFiredAt: &fired, CreatedAt: created,
	}
	if err := store.Save(ctx, tg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Get(ctx, "trg_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.TemplateID != "tpl_9" || got.TokenHash != "abc" ||
		got.LastFiredAt == nil || !got.LastFiredAt.Equal(fired) {
		t.Errorf("Get() = %+v, want the saved trigger", got)
	}

	if _, err := store.Get(ctx, "ghost"); !errors.Is(err, trigger.ErrNotFound) {
		t.Errorf("Get(ghost) error = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, "trg_1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(ctx, "trg_1"); !errors.Is(err, trigger.ErrNotFound) {
		t.Errorf("Delete(gone) error = %v, want ErrNotFound", err)
	}
}

// testFindByHash verifies token lookup and the miss case.
func testFindByHash(t *testing.T, store trigger.Store) {
	ctx := context.Background()
	if err := store.Save(ctx, &trigger.Trigger{
		ID: "trg_h", Name: "n", TemplateID: "tpl_1", TokenHash: "hash-xyz",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.FindByTokenHash(ctx, "hash-xyz")
	if err != nil {
		t.Fatalf("FindByTokenHash() error = %v", err)
	}
	if got.ID != "trg_h" {
		t.Errorf("FindByTokenHash() = %s, want trg_h", got.ID)
	}
	if _, err := store.FindByTokenHash(ctx, "nope"); !errors.Is(err, trigger.ErrNotFound) {
		t.Errorf("FindByTokenHash(miss) error = %v, want ErrNotFound", err)
	}
}

// testList verifies triggers come back oldest first.
func testList(t *testing.T, store trigger.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"trg_b", "trg_a"} {
		if err := store.Save(ctx, &trigger.Trigger{
			ID: id, Name: id, TemplateID: "tpl", TokenHash: id + "h",
			CreatedAt: base.Add(time.Duration(1-i) * time.Hour),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 2 || list[0].ID != "trg_a" || list[1].ID != "trg_b" {
		t.Errorf("List() order = %+v, want trg_a then trg_b", list)
	}
}
