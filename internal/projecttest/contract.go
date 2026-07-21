// Package projecttest provides a shared behavior contract for project.Store implementations so
// the in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package projecttest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dcadolph/switchtender/internal/project"
)

// Contract runs the project.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() project.Store) {
	t.Helper()
	t.Run("lifecycle", func(t *testing.T) { testLifecycle(t, newStore()) })
	t.Run("list ordered", func(t *testing.T) { testList(t, newStore()) })
	t.Run("update", func(t *testing.T) { testUpdate(t, newStore()) })
}

// testUpdate verifies an update changes the mutable fields, preserves the creation time, and
// reports ErrNotFound for an unknown id.
func testUpdate(t *testing.T, store project.Store) {
	ctx := context.Background()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Save(ctx, &project.Project{
		ID: "proj_1", Name: "old", RepoURL: "https://example.com/old.git",
		Branch: "main", InstallDeps: true, CreatedAt: created,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if err := store.Update(ctx, &project.Project{
		ID: "proj_1", Name: "new", RepoURL: "https://example.com/new.git",
		Branch: "dev", CredentialID: "cred_1", InstallDeps: false,
		Image: "img:1", PullCredentialID: "cred_2", OrgID: "org_new",
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	got, err := store.Get(ctx, "proj_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Name != "new" || got.RepoURL != "https://example.com/new.git" || got.Branch != "dev" ||
		got.CredentialID != "cred_1" || got.InstallDeps || got.Image != "img:1" ||
		got.PullCredentialID != "cred_2" || got.OrgID != "org_new" {
		t.Errorf("Get() = %+v, want the updated project", got)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want preserved %v", got.CreatedAt, created)
	}

	if err := store.Update(ctx, &project.Project{ID: "ghost", Name: "x", RepoURL: "r"}); !errors.Is(err, project.ErrNotFound) {
		t.Errorf("Update(ghost) error = %v, want ErrNotFound", err)
	}
}

// testLifecycle verifies a project round trips, updates, and deletes.
func testLifecycle(t *testing.T, store project.Store) {
	ctx := context.Background()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	p := &project.Project{
		ID: "proj_1", Name: "site", RepoURL: "ssh://git@example.com/site.git",
		Branch: "main", CredentialID: "cred_9", InstallDeps: true,
		Image: "quay.io/ansible/creator-ee:v0.1", PullCredentialID: "cred_reg",
		OrgID: "org_owner", CreatedAt: created,
	}
	if err := store.Save(ctx, p); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Get(ctx, "proj_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Name != "site" || got.RepoURL != p.RepoURL || got.Branch != "main" ||
		got.CredentialID != "cred_9" || !got.InstallDeps || got.Image != p.Image ||
		got.PullCredentialID != "cred_reg" || got.OrgID != "org_owner" || !got.CreatedAt.Equal(created) {
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
