// Package usertest provides a shared behavior contract for user.Store implementations so the
// in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package usertest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dcadolph/yardmaster/internal/user"
)

// Contract runs the user.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() user.Store) {
	t.Helper()
	t.Run("lifecycle", func(t *testing.T) { testLifecycle(t, newStore()) })
	t.Run("list ordered", func(t *testing.T) { testList(t, newStore()) })
}

// testLifecycle verifies a user round trips by id and username, updates, and deletes.
func testLifecycle(t *testing.T, store user.Store) {
	ctx := context.Background()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	u := &user.User{
		ID: "user_1", Username: "dispatcher", PasswordHash: "$2a$10$hash",
		Role: user.RoleOperator, CreatedAt: created,
	}
	if err := store.Save(ctx, u); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Get(ctx, "user_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Username != "dispatcher" || got.Role != user.RoleOperator ||
		got.PasswordHash != "$2a$10$hash" || !got.CreatedAt.Equal(created) {
		t.Errorf("Get() = %+v, want the saved user", got)
	}

	byName, err := store.FindByUsername(ctx, "dispatcher")
	if err != nil {
		t.Fatalf("FindByUsername() error = %v", err)
	}
	if byName.ID != "user_1" {
		t.Errorf("FindByUsername() = %s, want user_1", byName.ID)
	}
	if _, err := store.FindByUsername(ctx, "ghost"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("FindByUsername(ghost) error = %v, want ErrNotFound", err)
	}

	u.Role = user.RoleAdmin
	if err := store.Save(ctx, u); err != nil {
		t.Fatalf("Save() update error = %v", err)
	}
	got, _ = store.Get(ctx, "user_1")
	if got.Role != user.RoleAdmin {
		t.Errorf("Role after update = %s, want admin", got.Role)
	}

	if err := store.Delete(ctx, "user_1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(ctx, "user_1"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("Delete(gone) error = %v, want ErrNotFound", err)
	}
}

// testList verifies users come back oldest first.
func testList(t *testing.T, store user.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"user_b", "user_a"} {
		if err := store.Save(ctx, &user.User{
			ID: id, Username: id, PasswordHash: "h", Role: user.RoleViewer,
			CreatedAt: base.Add(time.Duration(1-i) * time.Hour),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 2 || list[0].ID != "user_a" || list[1].ID != "user_b" {
		t.Errorf("List() order = %+v, want user_a then user_b", list)
	}
}
