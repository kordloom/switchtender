package sqlitestore_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

// TestUpgradedDatabaseHealsThePolicyNotesColumn pins that a database from before runs recorded what
// a policy noted opens, takes a run carrying notes, and reads them back unchanged. Every save names
// the column, so an upgrade that missed it would fail every save outright.
func TestUpgradedDatabaseHealsThePolicyNotesColumn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db := openStoreAt(t, path)
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	rawExec(t, path, "ALTER TABLE runs DROP COLUMN policy_notes")

	store := openStoreAt(t, path).Runs()
	want := []string{"staging-advice (no change ticket, rego sha256:0123456789ab)"}
	if err := store.Save(ctx, &run.Run{ID: "run_upgraded", Playbook: "site.yml",
		Status: run.StatusPending, CreatedAt: baseTime, PolicyNotes: want}); err != nil {
		t.Fatalf("Save() on a healed database error = %v", err)
	}
	got, err := store.Get(ctx, "run_upgraded")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(want, got.PolicyNotes); diff != "" {
		t.Errorf("healed run notes mismatch (-want +got):\n%s", diff)
	}
}
