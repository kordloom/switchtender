// Package projecttest provides a shared behavior contract for project.Store implementations so
// the in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package projecttest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dcadolph/yardmaster/internal/project"
)

// Contract runs the project.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() project.Store) {
	t.Helper()
	t.Run("lifecycle", func(t *testing.T) { testLifecycle(t, newStore()) })
	t.Run("list ordered", func(t *testing.T) { testList(t, newStore()) })
}

// testLifecycle verifies a project round trips, updates, and deletes.
func testLifecycle(t *testing.T, store project.Store) {
	ctx := context.Background()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	p := &project.Project{
		ID: "proj_1", Name: "site", RepoURL: "ssh://git@example.com/site.git",
		Branch: "main", CredentialID: "cred_9", CreatedAt: created,
	}
	if err := store.Save(ctx, p); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Get(ctx, "proj_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Name != "site" || got.RepoURL != p.RepoURL || got.Branch != "main" ||
		got.CredentialID != "cred_9" || !got.CreatedAt.Equal(created) {
		t.Errorf("Get() = %+v, want the saved project", got)
	}

	if _, err := store.Get(ctx, "ghost"); !errors.Is(err, project.ErrNotFound) {
		t.Errorf("Get(ghost) error = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, "proj_1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(ctx, "proj_1"); !errors.Is(err, project.ErrNotFound) {
		t.Errorf("Delete(gone) error = %v, want ErrNotFound", err)
	}
}

// testList verifies projects come back oldest first.
func testList(t *testing.T, store project.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"proj_b", "proj_a"} {
		if err := store.Save(ctx, &project.Project{
			ID: id, Name: id, RepoURL: "r", CreatedAt: base.Add(time.Duration(1-i) * time.Hour),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 2 || list[0].ID != "proj_a" || list[1].ID != "proj_b" {
		t.Errorf("List() order = %+v, want proj_a then proj_b", list)
	}
}
