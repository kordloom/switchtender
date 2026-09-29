// Package orgtest provides a shared behavior contract for org.Store implementations so the in-memory,
// SQLite, and PostgreSQL backends cannot drift apart.
package orgtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/org"
)

// Contract runs the org.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() org.Store) {
	t.Helper()
	t.Run("lifecycle", func(t *testing.T) { testLifecycle(t, newStore()) })
	t.Run("membership", func(t *testing.T) { testMembership(t, newStore()) })
	t.Run("list and missing parent", func(t *testing.T) { testListAndMissingParent(t, newStore()) })
}

// testListAndMissingParent covers the two answers the backends disagreed about: what List returns,
// which had no test at all, and what happens when a membership names an organization that does not
// exist. Both SQL backends refuse the second on a foreign key while the in-memory store accepted it,
// and the in-memory store is the one every server and dispatch test runs against.
func testListAndMissingParent(t *testing.T, store org.Store) {
	ctx := context.Background()
	for _, name := range []string{"beta", "alpha"} {
		if err := store.Save(ctx, &org.Org{ID: "org_" + name, Name: name, CreatedAt: time.Now()}); err != nil {
			t.Fatalf("Save(%s) error = %v", name, err)
		}
	}
	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List() returned %d organizations, want 2", len(list))
	}
	for _, o := range list {
		if o.ID == "" || o.Name == "" {
			t.Errorf("List() returned an incomplete organization: %+v", o)
		}
	}

	if err := store.AddMember(ctx, "org_missing", "user_1", org.RoleMember); err == nil {
		t.Error("a membership was accepted for an organization that does not exist, so it hangs " +
			"off nothing; both SQL backends refuse this on a foreign key")
	}
}

// testLifecycle verifies an organization round trips and deletes, and unknown ids report ErrNotFound.
func testLifecycle(t *testing.T, store org.Store) {
	ctx := context.Background()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Save(ctx, &org.Org{ID: "org_1", Name: "acme", CreatedAt: created}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Get(ctx, "org_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Name != "acme" || !got.CreatedAt.Equal(created) {
		t.Errorf("Get() = %+v, want the saved org", got)
	}
	if _, err := store.Get(ctx, "ghost"); !errors.Is(err, org.ErrNotFound) {
		t.Errorf("Get(ghost) error = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, "org_1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(ctx, "org_1"); !errors.Is(err, org.ErrNotFound) {
		t.Errorf("Delete(gone) error = %v, want ErrNotFound", err)
	}
}

// testMembership verifies members are added with roles, a repeat add updates the role, membership
// lists both ways, and removal works.
func testMembership(t *testing.T, store org.Store) {
	ctx := context.Background()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Save(ctx, &org.Org{ID: "org_1", Name: "acme", CreatedAt: created}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.AddMember(ctx, "org_1", "user_a", org.RoleAdmin); err != nil {
		t.Fatalf("AddMember() error = %v", err)
	}
	if err := store.AddMember(ctx, "org_1", "user_b", org.RoleMember); err != nil {
		t.Fatalf("AddMember() error = %v", err)
	}
	members, err := store.Members(ctx, "org_1")
	if err != nil {
		t.Fatalf("Members() error = %v", err)
	}
	want := []org.Member{{UserID: "user_a", Role: org.RoleAdmin}, {UserID: "user_b", Role: org.RoleMember}}
	if diff := cmp.Diff(want, members); diff != "" {
		t.Errorf("Members() mismatch (-want +got):\n%s", diff)
	}
	// A repeated add updates the role rather than duplicating the member.
	if err := store.AddMember(ctx, "org_1", "user_b", org.RoleAdmin); err != nil {
		t.Fatalf("AddMember() error = %v", err)
	}
	forUser, err := store.OrgsForUser(ctx, "user_b")
	if err != nil {
		t.Fatalf("OrgsForUser() error = %v", err)
	}
	if diff := cmp.Diff([]org.Membership{{OrgID: "org_1", Role: org.RoleAdmin}}, forUser); diff != "" {
		t.Errorf("OrgsForUser() mismatch (-want +got):\n%s", diff)
	}
	if err := store.RemoveMember(ctx, "org_1", "user_a"); err != nil {
		t.Fatalf("RemoveMember() error = %v", err)
	}
	members, err = store.Members(ctx, "org_1")
	if err != nil {
		t.Fatalf("Members() error = %v", err)
	}
	if diff := cmp.Diff([]org.Member{{UserID: "user_b", Role: org.RoleAdmin}}, members); diff != "" {
		t.Errorf("Members() after remove mismatch (-want +got):\n%s", diff)
	}
}
