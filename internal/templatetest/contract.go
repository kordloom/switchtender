// Package templatetest provides a shared behavior contract for template.Store implementations so
// the in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package templatetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/template"
)

// Contract runs the template.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() template.Store) {
	t.Helper()
	t.Run("lifecycle", func(t *testing.T) { testLifecycle(t, newStore()) })
	t.Run("list ordered", func(t *testing.T) { testList(t, newStore()) })
	t.Run("update", func(t *testing.T) { testUpdate(t, newStore()) })
}

// testUpdate verifies an update rewrites every field, preserves the creation time, and reports
// ErrNotFound for an unknown id.
func testUpdate(t *testing.T, store template.Store) {
	ctx := context.Background()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Save(ctx, &template.Template{
		ID: "tpl_1", Name: "old", Playbook: "old.yml",
		CredentialIDs: []string{"cred_1"}, CreatedAt: created,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	want := &template.Template{
		ID: "tpl_1", Name: "new", ProjectID: "proj_2",
		Playbook: "plays/new.yml", Inventory: "hosts.ini", InventoryID: "inv_2", Shards: 4,
		Queue: "batch",
		Image: "ghcr.io/acme/ee:9", PullCredentialID: "cred_pull",
		CredentialIDs: []string{"cred_2", "cred_3"},
		ExtraVars:     map[string]any{"env": "stg"},
		Survey:        []template.SurveyField{{Var: "tier", Label: "Tier", Type: template.FieldText}},
		Tool:          "python", Command: "print('hi')", DryRun: true,
		OrgID:     "org_new",
		CreatedAt: created,
	}
	if err := store.Update(ctx, want); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	got, err := store.Get(ctx, "tpl_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("Get() after update mismatch (-want +got):\n%s", diff)
	}

	if err := store.Update(ctx, &template.Template{ID: "ghost", Name: "x", Playbook: "p.yml"}); !errors.Is(err, template.ErrNotFound) {
		t.Errorf("Update(ghost) error = %v, want ErrNotFound", err)
	}
}

// testLifecycle verifies a fully loaded template round trips and deletes.
func testLifecycle(t *testing.T, store template.Store) {
	ctx := context.Background()
	want := &template.Template{
		ID: "tpl_1", Name: "deploy", ProjectID: "proj_9",
		Playbook: "plays/site.yml", Inventory: "inventory.ini", InventoryID: "inv_7", Shards: 3,
		Image: "ghcr.io/acme/ee:8", PullCredentialID: "cred_pull",
		CredentialIDs: []string{"cred_1", "cred_2"},
		ExtraVars:     map[string]any{"env": "prod", "batch": float64(5)},
		Survey:        []template.SurveyField{{Var: "region", Label: "Region", Type: template.FieldChoice, Required: true, Choices: []string{"us", "eu"}}},
		Tool:          "bash", Command: "echo hi", DryRun: true,
		OrgID:     "org_owner",
		CreatedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := store.Save(ctx, want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Get(ctx, "tpl_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("Get() mismatch (-want +got):\n%s", diff)
	}

	if _, err := store.Get(ctx, "ghost"); !errors.Is(err, template.ErrNotFound) {
		t.Errorf("Get(ghost) error = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, "tpl_1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(ctx, "tpl_1"); !errors.Is(err, template.ErrNotFound) {
		t.Errorf("Delete(gone) error = %v, want ErrNotFound", err)
	}
}

// testList verifies templates come back oldest first.
func testList(t *testing.T, store template.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"tpl_b", "tpl_a"} {
		if err := store.Save(ctx, &template.Template{
			ID: id, Name: id, Playbook: "p.yml",
			CreatedAt: base.Add(time.Duration(1-i) * time.Hour),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 2 || list[0].ID != "tpl_a" || list[1].ID != "tpl_b" {
		t.Errorf("List() order = %+v, want tpl_a then tpl_b", list)
	}
}
