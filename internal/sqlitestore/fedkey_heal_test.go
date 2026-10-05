package sqlitestore_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/federation"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// TestFederationKeysGainTheirLifecycleColumns opens a database whose federation_keys table predates
// the activation and removal times, holding a key, and pins that the open adds both columns, that
// the key reads back activated at its creation, since the build that wrote it signed with every key
// from its creation, and with no removal, and that the times a rotation writes afterwards persist.
func TestFederationKeysGainTheirLifecycleColumns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "switchtender.db")
	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	for _, stmt := range []string{
		"ALTER TABLE federation_keys DROP COLUMN activated_at",
		"ALTER TABLE federation_keys DROP COLUMN removed_at",
		"INSERT INTO federation_keys (id, algorithm, public_key, sealed, created_at, retired_at) " +
			"VALUES ('kid_old', 'RS256', 'pub-old', 'sealed-old', '2026-10-01T09:00:00Z', '')",
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("simulate the earlier table, %s: %v", stmt, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	healed, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = healed.Close() })
	keys := healed.FederationKeys()
	got, err := keys.List(ctx)
	if err != nil {
		t.Fatalf("List() after the heal error = %v", err)
	}
	created := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	want := []*federation.Key{{
		ID: "kid_old", Algorithm: "RS256", PublicKey: "pub-old", Sealed: "sealed-old",
		CreatedAt: created, ActivatedAt: &created,
	}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("the earlier key after the heal (-want +got):\n%s", diff)
	}
	activated, removed := created.Add(time.Hour), created.Add(49*time.Hour)
	got[0].ActivatedAt, got[0].RemovedAt = &activated, &removed
	if err := keys.Save(ctx, got[0]); err != nil {
		t.Fatalf("Save() after the heal error = %v", err)
	}
	again, err := keys.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if diff := cmp.Diff(got, again); diff != "" {
		t.Errorf("the lifecycle times after a save (-want +got):\n%s", diff)
	}
}
