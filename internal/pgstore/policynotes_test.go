package pgstore

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

// TestOpenRestoresThePolicyNotesColumn pins that a PostgreSQL database from before runs recorded
// what a policy noted gains the column when it is opened, and then stores and reads notes whole.
// Every save names the column, so an upgrade that missed it would fail every run's save.
//
// It works on a database of its own, created and dropped here, because it removes a column.
func TestOpenRestoresThePolicyNotesColumn(t *testing.T) {
	t.Parallel()
	dsn := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
			t.Fatal("SWITCHTENDER_REQUIRE_FULL_SUITE is set and SWITCHTENDER_TEST_POSTGRES_DSN " +
				"is not, so the upgrade path for policy notes goes unchecked")
		}
		t.Skip("SWITCHTENDER_TEST_POSTGRES_DSN not set")
	}
	own := freshDatabase(t, dsn)
	db, err := Open(own)
	if err != nil {
		t.Fatalf("Open() on a new database error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := rawHandle(t, own).Exec("ALTER TABLE runs DROP COLUMN policy_notes"); err != nil {
		t.Fatalf("simulate a database from before the column: %v", err)
	}

	healed, err := Open(own)
	if err != nil {
		t.Fatalf("Open() on the older database error = %v", err)
	}
	t.Cleanup(func() { _ = healed.Close() })
	ctx := context.Background()
	want := []string{"staging-advice (no change ticket, rego sha256:0123456789ab)"}
	if err := healed.Runs().Save(ctx, &run.Run{ID: "run_noted", Playbook: "site.yml",
		Status: run.StatusPending, CreatedAt: time.Now(), PolicyNotes: want}); err != nil {
		t.Fatalf("Save() after the upgrade error = %v", err)
	}
	got, err := healed.Runs().Get(ctx, "run_noted")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(want, got.PolicyNotes); diff != "" {
		t.Errorf("notes after the upgrade mismatch (-want +got):\n%s", diff)
	}
}
