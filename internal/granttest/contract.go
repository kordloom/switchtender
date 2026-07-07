// Package granttest provides a shared behavior contract for grant.Store implementations so the
// in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package granttest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dcadolph/yardmaster/internal/grant"
)

// Contract runs the grant.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() grant.Store) {
	t.Helper()
	t.Run("lifecycle", func(t *testing.T) { testLifecycle(t, newStore()) })
	t.Run("for object", func(t *testing.T) { testForObject(t, newStore()) })
}

// testLifecycle verifies a grant round trips, lists oldest first, and deletes.
func testLifecycle(t *testing.T, store grant.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"grant_b", "grant_a"} {
		if err := store.Save(ctx, &grant.Grant{
			ID: id, Subject: "user_1", Object: "tpl_1", Access: grant.AccessUse,
			CreatedAt: base.Add(time.Duration(1-i) * time.Hour),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	got, err := store.Get(ctx, "grant_a")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Subject != "user_1" || got.Object != "tpl_1" || got.Access != grant.AccessUse {
		t.Errorf("Get() = %+v, want the saved grant", got)
	}
	if _, err := store.Get(ctx, "ghost"); !errors.Is(err, grant.ErrNotFound) {
		t.Errorf("Get(ghost) error = %v, want ErrNotFound", err)
	}

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 2 || list[0].ID != "grant_a" || list[1].ID != "grant_b" {
		t.Errorf("List() order = %+v, want grant_a then grant_b", list)
	}

	if err := store.Delete(ctx, "grant_a"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(ctx, "grant_a"); !errors.Is(err, grant.ErrNotFound) {
		t.Errorf("Delete(gone) error = %v, want ErrNotFound", err)
	}
}

// testForObject verifies grants filter by their object.
func testForObject(t *testing.T, store grant.Store) {
	ctx := context.Background()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	grants := []*grant.Grant{
		{ID: "grant_1", Subject: "user_a", Object: "tpl_x", Access: grant.AccessUse, CreatedAt: created},
		{ID: "grant_2", Subject: "team_1", Object: "tpl_x", Access: grant.AccessManage, CreatedAt: created.Add(time.Hour)},
		{ID: "grant_3", Subject: "user_b", Object: "proj_y", Access: grant.AccessUse, CreatedAt: created},
	}
	for _, g := range grants {
		if err := store.Save(ctx, g); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	onTpl, err := store.ForObject(ctx, "tpl_x")
	if err != nil {
		t.Fatalf("ForObject() error = %v", err)
	}
	if len(onTpl) != 2 || onTpl[0].ID != "grant_1" || onTpl[1].ID != "grant_2" {
		t.Errorf("ForObject(tpl_x) = %+v, want grant_1 then grant_2", onTpl)
	}

	none, err := store.ForObject(ctx, "cred_none")
	if err != nil {
		t.Fatalf("ForObject() error = %v", err)
	}
	if len(none) != 0 {
		t.Errorf("ForObject(cred_none) = %+v, want empty", none)
	}
}
