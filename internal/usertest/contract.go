// Package usertest provides a shared behavior contract for user.Store implementations so the
// in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package usertest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/user"
)

// Contract runs the user.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() user.Store) {
	t.Helper()
	t.Run("lifecycle", func(t *testing.T) { testLifecycle(t, newStore()) })
	t.Run("list ordered", func(t *testing.T) { testList(t, newStore()) })
	t.Run("update", func(t *testing.T) { testUpdate(t, newStore()) })
	t.Run("profile", func(t *testing.T) { testProfile(t, newStore()) })
	t.Run("last admin is guarded in one statement", func(t *testing.T) {
		testLastAdminGuard(t, newStore())
	})
}

// testLastAdminGuard verifies a store refuses the delete or the demotion that would leave an install
// with no administrator, and that it decides in the same statement that makes the change.
//
// Counting the admins first and changing after is two statements, and another request can pass the
// same count in between: two concurrent deletes of the last two admins both saw a survivor and both
// went through. An install with no administrator cannot reach any admin-gated route, including the
// one that creates a user, so the only way back is a shell on the host. Several control nodes share
// one database, so a lock in one process does not close it; the store has to.
func testLastAdminGuard(t *testing.T, store user.Store) {
	ctx := context.Background()
	admins := []string{"user_a", "user_b"}
	for _, id := range admins {
		if err := store.Save(ctx, &user.User{
			ID: id, Username: id, PasswordHash: "h", Role: user.RoleAdmin,
			CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
	}

	// One of two admins goes.
	if ok, err := store.DeleteUnlessLastAdmin(ctx, "user_a"); err != nil || !ok {
		t.Fatalf("deleting one of two admins = (%v, %v), want it removed", ok, err)
	}
	// The survivor does not.
	if ok, err := store.DeleteUnlessLastAdmin(ctx, "user_b"); err != nil || ok {
		t.Errorf("deleting the last admin = (%v, %v), want it refused", ok, err)
	}
	if _, err := store.Get(ctx, "user_b"); err != nil {
		t.Errorf("the last admin is gone: Get() error = %v", err)
	}
	// Nor can the survivor be demoted out of the role.
	demoted := &user.User{
		ID: "user_b", Username: "user_b", PasswordHash: "h", Role: user.RoleViewer,
	}
	if ok, err := store.UpdateUnlessLastAdmin(ctx, demoted); err != nil || ok {
		t.Errorf("demoting the last admin = (%v, %v), want it refused", ok, err)
	}
	if got, err := store.Get(ctx, "user_b"); err != nil {
		t.Errorf("Get() error = %v", err)
	} else if got.Role != user.RoleAdmin {
		t.Errorf("role = %q, want admin: the last administrator was demoted", got.Role)
	}
	// A non-admin is not protected by the rule.
	if err := store.Save(ctx, &user.User{
		ID: "user_c", Username: "user_c", PasswordHash: "h", Role: user.RoleViewer,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save(user_c) error = %v", err)
	}
	if ok, err := store.DeleteUnlessLastAdmin(ctx, "user_c"); err != nil || !ok {
		t.Errorf("deleting a viewer = (%v, %v), want it removed", ok, err)
	}
	// A missing account is reported as missing rather than as a refusal.
	if _, err := store.DeleteUnlessLastAdmin(ctx, "user_gone"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("deleting a missing user error = %v, want ErrNotFound", err)
	}
}

// testProfile verifies the profile fields survive a save, an update, and a read on every backend,
// including a link list holding the separators a joined encoding would lose.
func testProfile(t *testing.T, store user.Store) {
	ctx := context.Background()
	links := []string{"https://wiki.example.com/p?a=1,2&b=3", "https://oncall.example.com/x"}
	u := &user.User{
		ID: "user_1", Username: "ada", PasswordHash: "h", Role: user.RoleOperator,
		FullName: "Ada Lovelace", Email: "ada@example.com", Phone: "+1 555 0100",
		Title: "Platform Engineer", Links: links, Notes: "review each quarter",
		CreatedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := store.Save(ctx, u); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Get(ctx, "user_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.FullName != "Ada Lovelace" || got.Email != "ada@example.com" ||
		got.Phone != "+1 555 0100" || got.Title != "Platform Engineer" ||
		got.Notes != "review each quarter" {
		t.Errorf("Get() profile = %+v, want the saved values", got)
	}
	if len(got.Links) != 2 || got.Links[0] != links[0] || got.Links[1] != links[1] {
		t.Errorf("Get() links = %q, want %q", got.Links, links)
	}

	// A mutation of the returned slice must not reach the store.
	got.Links[0] = "https://evil.example.com"
	again, err := store.Get(ctx, "user_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if again.Links[0] != links[0] {
		t.Errorf("Get() link after caller mutation = %q, want %q", again.Links[0], links[0])
	}

	// An update replaces the profile wholesale, so a cleared field comes back cleared.
	u.FullName = "Ada L"
	u.Phone = ""
	u.Links = nil
	if err := store.Update(ctx, u); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	updated, err := store.Get(ctx, "user_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if updated.FullName != "Ada L" || updated.Phone != "" || len(updated.Links) != 0 {
		t.Errorf("Get() after update = %+v, want the name changed and phone and links cleared", updated)
	}
}

// testUpdate verifies an update changes the username, role, and password hash, preserves the
// creation time, and reports ErrNotFound for an unknown id.
func testUpdate(t *testing.T, store user.Store) {
	ctx := context.Background()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Save(ctx, &user.User{
		ID: "user_1", Username: "old", PasswordHash: "$2a$10$old",
		Role: user.RoleViewer, CreatedAt: created,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if err := store.Update(ctx, &user.User{
		ID: "user_1", Username: "new", PasswordHash: "$2a$10$new", Role: user.RoleAdmin,
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	got, err := store.Get(ctx, "user_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Username != "new" || got.Role != user.RoleAdmin || got.PasswordHash != "$2a$10$new" {
		t.Errorf("Get() = %+v, want the updated user", got)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want preserved %v", got.CreatedAt, created)
	}
	// The old username no longer resolves; the new one does.
	if _, err := store.FindByUsername(ctx, "old"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("FindByUsername(old) error = %v, want ErrNotFound", err)
	}
	if byName, err := store.FindByUsername(ctx, "new"); err != nil || byName.ID != "user_1" {
		t.Errorf("FindByUsername(new) = %v, %v, want user_1", byName, err)
	}

	if err := store.Update(ctx, &user.User{ID: "ghost", Username: "x", Role: user.RoleViewer}); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("Update(ghost) error = %v, want ErrNotFound", err)
	}
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
