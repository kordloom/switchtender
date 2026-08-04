// Package credtest provides a shared behavior contract for credential.Store implementations so
// the in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package credtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/credential"
)

// Contract runs the credential.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() credential.Store) {
	t.Helper()
	t.Run("lifecycle", func(t *testing.T) { testLifecycle(t, newStore()) })
	t.Run("list ordered", func(t *testing.T) { testList(t, newStore()) })
	t.Run("update", func(t *testing.T) { testUpdate(t, newStore()) })
}

// testUpdate verifies an update changes the name, kind, and sealed secret, preserves the creation
// time, and reports ErrNotFound for an unknown id.
func testUpdate(t *testing.T, store credential.Store) {
	ctx := context.Background()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Save(ctx, &credential.Credential{
		ID: "cred_1", Name: "old", Kind: credential.KindSSHKey, Secret: "sealed-old",
		Source: credential.SourceLocal, CreatedAt: created,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if err := store.Update(ctx, &credential.Credential{
		ID: "cred_1", Name: "new", Kind: credential.KindVaultPassword, Secret: "sealed-new",
		Source: credential.SourceCommand, OrgID: "org_new", VaultID: "staging",
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	got, err := store.Get(ctx, "cred_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Name != "new" || got.Kind != credential.KindVaultPassword || got.Secret != "sealed-new" {
		t.Errorf("Get() = %+v, want the updated credential", got)
	}
	if got.OrgID != "org_new" {
		t.Errorf("OrgID after update = %q, want org_new", got.OrgID)
	}
	if got.VaultID != "staging" {
		t.Errorf("VaultID after update = %q, want the relabel persisted", got.VaultID)
	}
	if got.Source != credential.SourceCommand {
		t.Errorf("Source = %q, want %q", got.Source, credential.SourceCommand)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want preserved %v", got.CreatedAt, created)
	}

	if err := store.Update(ctx, &credential.Credential{ID: "ghost", Name: "x", Kind: credential.KindSSHKey, Secret: "s"}); !errors.Is(err, credential.ErrNotFound) {
		t.Errorf("Update(ghost) error = %v, want ErrNotFound", err)
	}
}

// testLifecycle verifies a credential round trips with its sealed secret, updates, and deletes.
func testLifecycle(t *testing.T, store credential.Store) {
	ctx := context.Background()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	c := &credential.Credential{
		ID: "cred_1", Name: "fleet-vault", Kind: credential.KindVaultPassword,
		Secret: "sealed-bytes", Source: credential.SourceCommand, OrgID: "org_owner",
		VaultID: "prod", CreatedAt: created,
	}
	if err := store.Save(ctx, c); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Get(ctx, "cred_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Name != "fleet-vault" || got.Kind != credential.KindVaultPassword || got.Secret != "sealed-bytes" {
		t.Errorf("Get() = %+v, want the saved credential with its sealed secret", got)
	}
	if got.VaultID != "prod" {
		t.Errorf("VaultID = %q, want prod carried through the store", got.VaultID)
	}
	if got.OrgID != "org_owner" {
		t.Errorf("OrgID = %q, want org_owner", got.OrgID)
	}
	if got.Source != credential.SourceCommand {
		t.Errorf("Source = %q, want %q", got.Source, credential.SourceCommand)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, created)
	}

	if _, err := store.Get(ctx, "ghost"); !errors.Is(err, credential.ErrNotFound) {
		t.Errorf("Get(ghost) error = %v, want ErrNotFound", err)
	}

	if err := store.Delete(ctx, "cred_1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(ctx, "cred_1"); !errors.Is(err, credential.ErrNotFound) {
		t.Errorf("Delete(gone) error = %v, want ErrNotFound", err)
	}
}

// testList verifies credentials come back oldest first.
func testList(t *testing.T, store credential.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"cred_b", "cred_a"} {
		if err := store.Save(ctx, &credential.Credential{
			ID: id, Name: id, Kind: credential.KindVaultPassword, Secret: "s",
			CreatedAt: base.Add(time.Duration(1-i) * time.Hour),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 2 || list[0].ID != "cred_a" || list[1].ID != "cred_b" {
		t.Errorf("List() order = %+v, want cred_a then cred_b", list)
	}
}
