package pgstore_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/pgstore"
	"github.com/kordloom/switchtender/internal/run"
)

// TestOpenHealsBeforeTheSchemaIndexes pins the order the Postgres migration runs in. The schema blob's
// own CREATE INDEX statements reference columns only the heal adds: idx_runs_pending_claim covers
// queue, and a database from before that column, which the hand ALTERs deleted in 2026-07 prove
// exists, failed the blob with "column does not exist" and aborted the migration transaction before
// the heal it needed ever ran. Open failed on every restart with no self-recovery. Healing runs first
// now, exactly as the SQLite store orders it and for the same reason.
func TestOpenHealsBeforeTheSchemaIndexes(t *testing.T) {
	dsn := testDSN(t)
	// Start from the current schema, then strip a column the blob indexes over, the way a pre-queue
	// database stands. The partial index over it goes first, since Postgres will not drop a column a
	// dependent index survives without CASCADE, and CASCADE is exactly what a real old database never
	// ran either.
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	for _, stmt := range []string{
		"DROP INDEX IF EXISTS idx_runs_pending_claim",
		"ALTER TABLE runs DROP COLUMN IF EXISTS queue",
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("simulate pre-queue database, %s: %v", stmt, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	healed, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("reopen after downgrade: %v — the heal ran on the wrong side of the schema exec, "+
			"so the index over the missing column aborted the migration before the heal could add it", err)
	}
	t.Cleanup(func() { _ = healed.Close() })

	// The column is back and usable, which is what an upgraded deployment actually needs.
	ctx := context.Background()
	if err := healed.Runs().Save(ctx, &run.Run{
		ID: "run_healorder", Status: run.StatusPending, CreatedAt: time.Now(),
		Tool: "bash", Command: "echo hi", Queue: "default",
	}); err != nil {
		t.Fatalf("Save after heal: %v", err)
	}
	got, err := healed.Runs().Get(ctx, "run_healorder")
	if err != nil {
		t.Fatalf("Get after heal: %v", err)
	}
	if got.Queue != "default" {
		t.Errorf("queue after heal = %q, want default", got.Queue)
	}
}
