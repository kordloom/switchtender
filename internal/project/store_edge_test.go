package project_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/project"
)

// TestMemStoreCopiesOnEveryBoundary pins that the store hands out copies rather than the objects it
// holds.
//
// A project carries the remote the executor clones and the credential it clones with. If Save kept
// the caller's pointer, or Get handed one back, a handler editing the value it just read would
// repoint a stored project without an update ever being made, and nothing in the audit trail would
// record it. The copy is the only thing that makes the store's contents match what was written.
func TestMemStoreCopiesOnEveryBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := project.NewMemStore()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	original := &project.Project{
		ID: "proj_1", Name: "site", RepoURL: "ssh://git@example.com/site.git",
		Branch: "main", CredentialID: "cred_9", InstallDeps: true, CreatedAt: created,
	}
	if err := store.Save(ctx, original); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// Test 0: mutating the struct the caller saved must not reach the store.
	original.RepoURL = "ssh://git@attacker.example/evil.git"
	original.CredentialID = "cred_stolen"
	got, err := store.Get(ctx, "proj_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.RepoURL != "ssh://git@example.com/site.git" || got.CredentialID != "cred_9" {
		t.Errorf("the store aliased the saved struct, RepoURL = %q credential = %q",
			got.RepoURL, got.CredentialID)
	}

	// Test 1: mutating the struct Get returned must not reach the store either.
	got.RepoURL = "ssh://git@attacker.example/evil.git"
	again, err := store.Get(ctx, "proj_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if again.RepoURL != "ssh://git@example.com/site.git" {
		t.Errorf("the store aliased the value it returned, RepoURL = %q", again.RepoURL)
	}

	// Test 2: the same holds for the values List returns.
	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List() returned %d projects, want 1", len(list))
	}
	list[0].Branch = "attacker"
	final, err := store.Get(ctx, "proj_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if final.Branch != "main" {
		t.Errorf("the store aliased a listed value, Branch = %q", final.Branch)
	}
}

// TestMemStoreListOrdering pins the documented order and its tie-break. Two projects created in the
// same instant, which a seeded install produces, must still come back in one stable order rather
// than whichever order the map happened to iterate in.
func TestMemStoreListOrdering(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		// Name labels the ordering case.
		Name string
		// Saved are the projects written, in the order they are written.
		Saved []*project.Project
		// WantIDs is the order List must answer with.
		WantIDs []string
	}{{ // Test 0: An empty store lists nothing rather than failing.
		Name: "empty", Saved: nil, WantIDs: nil,
	}, { // Test 1: Oldest first, regardless of insertion order.
		Name: "oldest first",
		Saved: []*project.Project{
			{ID: "proj_c", CreatedAt: base.Add(2 * time.Hour)},
			{ID: "proj_a", CreatedAt: base},
			{ID: "proj_b", CreatedAt: base.Add(time.Hour)},
		},
		WantIDs: []string{"proj_a", "proj_b", "proj_c"},
	}, { // Test 2: Identical creation times break the tie on id, so the order is stable.
		Name: "tie on id",
		Saved: []*project.Project{
			{ID: "proj_z", CreatedAt: base},
			{ID: "proj_m", CreatedAt: base},
			{ID: "proj_a", CreatedAt: base},
		},
		WantIDs: []string{"proj_a", "proj_m", "proj_z"},
	}, { // Test 3: A zero creation time sorts before a stamped one rather than being dropped.
		Name: "zero time",
		Saved: []*project.Project{
			{ID: "proj_stamped", CreatedAt: base},
			{ID: "proj_zero"},
		},
		WantIDs: []string{"proj_zero", "proj_stamped"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := project.NewMemStore()
			for _, p := range test.Saved {
				if err := store.Save(ctx, p); err != nil {
					t.Fatalf("Save(%s) error = %v", p.ID, err)
				}
			}
			list, err := store.List(ctx)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			var ids []string
			for _, p := range list {
				ids = append(ids, p.ID)
			}
			if diff := cmp.Diff(test.WantIDs, ids, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("List() order mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestMemStoreSaveReplacesAndUpdatePreservesCreation pins the difference between the two writes.
// Save is an upsert that takes the creation time it is given, and Update keeps the stored one, so an
// edit made through the API cannot silently restamp when a project came into existence.
func TestMemStoreSaveReplacesAndUpdatePreservesCreation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := project.NewMemStore()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Save(ctx, &project.Project{ID: "proj_1", Name: "one", CreatedAt: created}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// Test 0: Save with the same id replaces wholesale, including the creation time.
	replaced := created.Add(48 * time.Hour)
	if err := store.Save(ctx, &project.Project{ID: "proj_1", Name: "two", CreatedAt: replaced}); err != nil {
		t.Fatalf("Save() replace error = %v", err)
	}
	got, err := store.Get(ctx, "proj_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Name != "two" || !got.CreatedAt.Equal(replaced) {
		t.Errorf("Save() did not replace, got %+v", got)
	}

	// Test 1: Update keeps the stored creation time even when the caller supplies one.
	if err := store.Update(ctx, &project.Project{
		ID: "proj_1", Name: "three", CreatedAt: created.Add(-100 * time.Hour),
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	got, err = store.Get(ctx, "proj_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Name != "three" || !got.CreatedAt.Equal(replaced) {
		t.Errorf("Update() did not preserve the creation time, got %+v", got)
	}

	// Test 2: Update and Delete on an unknown id refuse rather than creating anything.
	if err := store.Update(ctx, &project.Project{ID: "proj_missing"}); !errors.Is(err, project.ErrNotFound) {
		t.Errorf("Update(missing) error = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, "proj_missing"); !errors.Is(err, project.ErrNotFound) {
		t.Errorf("Delete(missing) error = %v, want ErrNotFound", err)
	}
	if _, err := store.Get(ctx, "proj_missing"); !errors.Is(err, project.ErrNotFound) {
		t.Errorf("Get(missing) error = %v, want ErrNotFound", err)
	}
	if _, err := store.Get(ctx, ""); !errors.Is(err, project.ErrNotFound) {
		t.Errorf("Get(empty) error = %v, want ErrNotFound", err)
	}
}

// TestMemStoreIsSafeForConcurrentUse exercises the store the way the server does, from many
// goroutines at once. The Store interface requires implementations to be safe for concurrent use,
// and the API serves reads and writes on separate connections, so a data race here is a crash in
// production rather than a test detail. Run with -race for this to mean anything.
func TestMemStoreIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := project.NewMemStore()
	const workers = 16
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := fmt.Sprintf("proj_%02d", n)
			p := &project.Project{ID: id, Name: id, RepoURL: "https://example.com/x.git"}
			if err := store.Save(ctx, p); err != nil {
				t.Errorf("Save(%s) error = %v", id, err)
				return
			}
			if _, err := store.Get(ctx, id); err != nil {
				t.Errorf("Get(%s) error = %v", id, err)
			}
			if _, err := store.List(ctx); err != nil {
				t.Errorf("List() error = %v", err)
			}
			p.Branch = "main"
			if err := store.Update(ctx, p); err != nil {
				t.Errorf("Update(%s) error = %v", id, err)
			}
		}(i)
	}
	wg.Wait()

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != workers {
		t.Errorf("List() returned %d projects, want %d", len(list), workers)
	}
}

// TestNewIDShape pins that generated project ids carry the prefix the browse guard and the run
// records depend on, and that two calls do not collide. A checkout directory is named by this id, so
// a value carrying a path separator would place a checkout outside the cache.
func TestNewIDShape(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool, 256)
	for range 256 {
		id := project.NewID()
		if !strings.HasPrefix(id, "proj_") {
			t.Fatalf("NewID() = %q, want the proj_ prefix", id)
		}
		if strings.ContainsAny(id, `/\.`) {
			t.Fatalf("NewID() = %q, which cannot be used as a checkout directory name", id)
		}
		if seen[id] {
			t.Fatalf("NewID() returned %q twice", id)
		}
		seen[id] = true
	}
}
