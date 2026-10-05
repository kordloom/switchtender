package pgstore

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// upgRunKeyedTables are the tables this release keeps a run's rows in that the release it upgrades
// from does not know exist, so that release's retention purge never deletes from them.
var upgRunKeyedTables = []string{
	"run_decisions", "notification_events", "notification_deliveries", "review_reports",
}

// upgPreviousPurge is the retention purge of the release this one upgrades from, statement for
// statement as internal/pgstore/purge.go held it at v1.102.0, with its batching left out: it
// deletes a terminal run's events and logs, keeps its readability decision, and deletes the run.
// It knows nothing of the tables above.
var upgPreviousPurge = []string{
	`DELETE FROM run_events WHERE run_id IN (
	SELECT id FROM runs WHERE ` + terminalRun + ` AND created_at < $1)`,
	`DELETE FROM run_logs WHERE run_id IN (
	SELECT id FROM runs WHERE ` + terminalRun + ` AND created_at < $1)`,
	`INSERT INTO run_auth (run_id, org_id, project_id, inventory_id, pull_credential_id,
	credential_ids)
SELECT id, org_id, project_id, inventory_id, pull_credential_id, credential_ids
FROM runs WHERE ` + terminalRun + ` AND created_at < $1
ON CONFLICT (run_id) DO NOTHING`,
	`DELETE FROM runs WHERE id IN (SELECT id FROM runs WHERE ` + terminalRun + ` AND created_at < $1)`,
}

// TestUpgradePurgeTakesTheRowsAnEarlierReleasesPurgeLeft gives a finished run an approver's
// decision record with its reason, a notification event and delivery, and a review report, then
// runs the previous release's retention purge over it, which is what happens when that release
// serves the database during a rolling update or after a rollback with retention configured, since
// its sweeper runs once at start. That purge deletes the run and leaves every one of those rows
// behind.
//
// This release's retention then never removes them. Its purge deletes a table's rows by joining to
// the terminal runs it is about to delete, and the run is already gone, so the reason text, which
// the purge comment says goes with its run, and the redacted run snapshot stay forever. The rows
// also keep the run's id referenced, so its retained readability decision is never dropped either.
// One retention sweep of this release has to leave nothing of a run that no longer exists.
func TestUpgradePurgeTakesTheRowsAnEarlierReleasesPurgeLeft(t *testing.T) {
	shared := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if shared == "" {
		skipOrFail(t, "SWITCHTENDER_TEST_POSTGRES_DSN not set")
	}
	dsn := freshDatabase(t, shared)
	ctx := context.Background()
	db, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const id = "run_upg_purged"
	created := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	ended := created.Add(time.Minute)
	if err := db.Runs().Save(ctx, &run.Run{ID: id, Playbook: "site.yml",
		Status: run.StatusSucceeded, CreatedAt: created, EndedAt: &ended}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	raw := rawHandle(t, dsn)
	for _, stmt := range []string{
		`INSERT INTO run_decisions (id, kind, decision_id, run_id, verdict, recorded_at, actor,
			reason_text, reason_random, reason_commitment)
		VALUES ('aud_upg', 'decision', 'aud_upg', 'run_upg_purged', 'approved',
			'2026-01-05T09:00:30Z', 'approver', 'call the on-call lead first', '00ff', 'sha256:00')`,
		`INSERT INTO notification_events (run_id, seq, event, dedupe_key, snapshot, created_ms)
		VALUES ('run_upg_purged', 1, 'success', 'success', '{}', 1767603660000)`,
		`INSERT INTO notification_deliveries (notification_id, run_id, seq, event, status,
			created_ms)
		VALUES ('ntf_upg', 'run_upg_purged', 1, 'success', 'failed', 1767603660000)`,
		`INSERT INTO review_reports (id, run_id, created_at, done)
		VALUES ('run_upg_purged', 'run_upg_purged', '2026-01-05T09:00:00Z', 1)`,
	} {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("record the run's rows, %s: %v", stmt, err)
		}
	}

	cutoff := created.Add(24 * time.Hour)
	for _, stmt := range upgPreviousPurge {
		if _, err := raw.ExecContext(ctx, stmt, sqlutil.FormatTime(cutoff)); err != nil {
			t.Fatalf("the previous release's purge, %s: %v", stmt, err)
		}
	}
	if _, err := db.Runs().Get(ctx, id); err == nil {
		t.Fatalf("the previous release's purge left the run in place, so nothing is tested")
	}

	// One retention sweep of this release, in the order its sweeper runs one.
	if _, err := db.Runs().PurgeRunsBefore(ctx, cutoff); err != nil {
		t.Fatalf("PurgeRunsBefore() error = %v", err)
	}
	if _, err := db.Runs().PurgeRunAuth(ctx); err != nil {
		t.Fatalf("PurgeRunAuth() error = %v", err)
	}

	left := map[string]int{}
	for _, table := range append(upgRunKeyedTables, "run_auth") {
		var n int
		if err := raw.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE run_id = $1",
			id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n > 0 {
			left[table] = n
		}
	}
	if diff := cmp.Diff(map[string]int{}, left, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("rows of a purged run that this release's retention kept (-want +got):\n%s", diff)
	}
}
