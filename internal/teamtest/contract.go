// Package teamtest provides a shared behavior contract for team.Store implementations so the
// in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package teamtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/dcadolph/yardmaster/internal/team"
)

// Contract runs the team.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() team.Store) {
	t.Helper()
	t.Run("lifecycle", func(t *testing.T) { testLifecycle(t, newStore()) })
	t.Run("membership", func(t *testing.T) { testMembership(t, newStore()) })
}

// testLifecycle verifies a team round trips, lists oldest first, and deletes.
func testLifecycle(t *testing.T, store team.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"team_b", "team_a"} {
		if err := store.Save(ctx, &team.Team{
			ID: id, Name: id, CreatedAt: base.Add(time.Duration(1-i) * time.Hour),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	got, err := store.Get(ctx, "team_a")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Name != "team_a" {
		t.Errorf("Get() = %+v, want team_a", got)
	}
	if _, err := store.Get(ctx, "ghost"); !errors.Is(err, team.ErrNotFound) {
		t.Errorf("Get(ghost) error = %v, want ErrNotFound", err)
	}

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 2 || list[0].ID != "team_a" || list[1].ID != "team_b" {
		t.Errorf("List() order = %+v, want team_a then team_b", list)
	}

	if err := store.Delete(ctx, "team_a"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(ctx, "team_a"); !errors.Is(err, team.ErrNotFound) {
		t.Errorf("Delete(gone) error = %v, want ErrNotFound", err)
	}
}

// testMembership verifies members add, list, resolve by user, and drop with the team.
func testMembership(t *testing.T, store team.Store) {
	ctx := context.Background()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Save(ctx, &team.Team{ID: "team_1", Name: "ops", CreatedAt: created}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	for _, u := range []string{"user_b", "user_a", "user_a"} {
		if err := store.AddMember(ctx, "team_1", u); err != nil {
			t.Fatalf("AddMember() error = %v", err)
		}
	}
	members, err := store.Members(ctx, "team_1")
	if err != nil {
		t.Fatalf("Members() error = %v", err)
	}
	if diff := cmp.Diff([]string{"user_a", "user_b"}, members); diff != "" {
		t.Errorf("Members() mismatch (-want +got):\n%s", diff)
	}

	teams, err := store.TeamsForUser(ctx, "user_a")
	if err != nil {
		t.Fatalf("TeamsForUser() error = %v", err)
	}
	if diff := cmp.Diff([]string{"team_1"}, teams); diff != "" {
		t.Errorf("TeamsForUser() mismatch (-want +got):\n%s", diff)
	}

	if err := store.RemoveMember(ctx, "team_1", "user_a"); err != nil {
		t.Fatalf("RemoveMember() error = %v", err)
	}
	members, _ = store.Members(ctx, "team_1")
	if diff := cmp.Diff([]string{"user_b"}, members); diff != "" {
		t.Errorf("Members() after remove mismatch (-want +got):\n%s", diff)
	}

	// Deleting the team drops its memberships.
	if err := store.Delete(ctx, "team_1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	teams, _ = store.TeamsForUser(ctx, "user_b")
	if len(teams) != 0 {
		t.Errorf("TeamsForUser() after team delete = %v, want none", teams)
	}
}
