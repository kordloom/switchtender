package sqlitestore_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// upgTerminal is the terminal-run predicate both releases' purges select with.
const upgTerminal = "status IN ('succeeded', 'failed', 'canceled', 'interrupted', 'rejected')"

// upgPreviousPurge returns the retention purge of the release this one upgrades from, statement
// for statement as internal/sqlitestore/purge.go held it at v1.102.0 with its batching left out,
// for runs created before cut. It knows nothing of the tables this release added.
func upgPreviousPurge(cut string) []string {
	older := "SELECT id FROM runs WHERE " + upgTerminal + " AND created_at < '" + cut + "'"
	return []string{
		"DELETE FROM run_events WHERE run_id IN (" + older + ")",
		"DELETE FROM run_logs WHERE run_id IN (" + older + ")",
		`INSERT OR IGNORE INTO run_auth (run_id, org_id, project_id, inventory_id,
			pull_credential_id, credential_ids)
		SELECT id, org_id, project_id, inventory_id, pull_credential_id, credential_ids
		FROM runs WHERE ` + upgTerminal + " AND created_at < '" + cut + "'",
		"DELETE FROM runs WHERE id IN (" + older + ")",
	}
}

// TestUpgradePurgeTakesTheRowsAnEarlierReleasesPurgeLeft gives a finished run an approver's
// decision record with its reason, a notification event and delivery, and a review report, then
// runs the previous release's retention purge over it, which is what happens when a rollback puts
// that release on the file with retention configured, since its sweeper runs once at start. That
// purge deletes the run and leaves every one of those rows behind.
//
// This release's retention then never removes them. Its purge deletes a table's rows by joining to
// the terminal runs it is about to delete, and the run is already gone, so the reason text, which
// the purge comment says goes with its run, and the redacted run snapshot stay forever, and they
// keep the run's retained readability decision alive with them. One retention sweep of this
// release has to leave nothing of a run that no longer exists.
func TestUpgradePurgeTakesTheRowsAnEarlierReleasesPurgeLeft(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "switchtender.db")
	db := openStoreAt(t, path)

	const id = "run_upg_purged"
	created := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	ended := created.Add(time.Minute)
	if err := db.Runs().Save(ctx, &run.Run{ID: id, Playbook: "site.yml",
		Status: run.StatusSucceeded, CreatedAt: created, EndedAt: &ended}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := db.Decisions().Save(ctx, &decision.Record{ID: "aud_upg", Kind: decision.KindDecision,
		DecisionID: "aud_upg", RunID: id, Verdict: "approved", At: created.Add(30 * time.Second),
		Actor: "approver", Reason: &decision.Reason{Text: "call the on-call lead first",
			Random: "00ff", Commitment: "sha256:00"}}); err != nil {
		t.Fatalf("Save(decision) error = %v", err)
	}
	rawExec(t, path,
		`INSERT INTO notification_events (run_id, seq, event, dedupe_key, snapshot, created_ms)
		VALUES ('run_upg_purged', 1, 'success', 'success', '{}', 1767603660000)`,
		`INSERT INTO notification_deliveries (notification_id, run_id, seq, event, status,
			created_ms)
		VALUES ('ntf_upg', 'run_upg_purged', 1, 'success', 'failed', 1767603660000)`,
		`INSERT INTO review_reports (id, run_id, created_at, done)
		VALUES ('run_upg_purged', 'run_upg_purged', '2026-01-05T09:00:00Z', 1)`,
	)

	cutoff := created.Add(24 * time.Hour)
	rawExec(t, path, upgPreviousPurge(sqlutil.FormatTime(cutoff))...)
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

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw %s: %v", path, err)
	}
	defer func() { _ = raw.Close() }()
	left := map[string]int{}
	for _, table := range []string{"run_decisions", "notification_events",
		"notification_deliveries", "review_reports", "run_auth"} {
		var n int
		q := "SELECT count(*) FROM " + table + " WHERE run_id = ?"
		if err := raw.QueryRowContext(ctx, q, id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n > 0 {
			left[table] = n
		}
	}
	if diff := cmp.Diff(map[string]int{}, left, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("rows of a purged run that this release's retention kept (-want +got):\n%s",
			strings.TrimSpace(diff))
	}
}
