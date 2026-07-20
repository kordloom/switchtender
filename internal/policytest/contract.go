// Package policytest provides a shared behavior contract for policy.Store implementations so the
// in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package policytest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dcadolph/switchtender/internal/policy"
)

// Contract runs the policy.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() policy.Store) {
	t.Helper()
	t.Run("save list delete", func(t *testing.T) { testSaveListDelete(t, newStore()) })
	t.Run("get", func(t *testing.T) { testGet(t, newStore()) })
	t.Run("empty list is non-nil", func(t *testing.T) {
		got, err := newStore().List(context.Background())
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if got == nil {
			t.Error("List() on an empty store = nil, want a non-nil empty slice")
		}
	})
}

// testGet verifies a saved policy round-trips through Get and a missing id reports ErrNotFound.
func testGet(t *testing.T, store policy.Store) {
	ctx := context.Background()
	p := &policy.Policy{
		ID: policy.NewID(), Name: "prod-destroy", Tool: "terraform", CommandContains: "destroy",
		InventoryID: "inv_prod", ExcludeDryRun: true,
		CreatedAt: time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC),
	}
	if err := store.Save(ctx, p); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Get(ctx, p.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Name != "prod-destroy" || got.Tool != "terraform" || got.CommandContains != "destroy" ||
		got.InventoryID != "inv_prod" || !got.ExcludeDryRun {
		t.Errorf("Get() = %+v, want the saved policy", got)
	}
	if _, err := store.Get(ctx, "pol_missing"); !errors.Is(err, policy.ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}
}

// testSaveListDelete verifies policies round-trip, list oldest first, and delete reports a missing id.
func testSaveListDelete(t *testing.T, store policy.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	for i, name := range []string{"prod-destroy", "all-terraform"} {
		if err := store.Save(ctx, &policy.Policy{
			ID: policy.NewID(), Name: name, Tool: "terraform", CommandContains: "destroy",
			InventoryID: "inv_prod", ExcludeDryRun: true,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	all, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(all) != 2 || all[0].Name != "prod-destroy" {
		t.Fatalf("List() = %+v, want two policies oldest first", all)
	}
	if all[0].Tool != "terraform" || all[0].CommandContains != "destroy" ||
		all[0].InventoryID != "inv_prod" || !all[0].ExcludeDryRun {
		t.Errorf("policy fields did not round-trip: %+v", all[0])
	}

	if err := store.Delete(ctx, all[0].ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	rest, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(rest) != 1 || rest[0].Name != "all-terraform" {
		t.Errorf("after delete = %+v, want one policy", rest)
	}
	if err := store.Delete(ctx, "pol_missing"); !errors.Is(err, policy.ErrNotFound) {
		t.Errorf("Delete(missing) = %v, want ErrNotFound", err)
	}
}
