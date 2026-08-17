package sqlitestore_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// tableColumns reads a table's column names straight from SQLite.
func tableColumns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TestOpenHealsEveryMissingColumn is the general form of a bug that reached the public demo. The
// migration was a hand-maintained list of ALTER statements beside a schema that kept growing, and the
// two drifted: runs.org_id was added to the CREATE and to the shared select list and never to the
// migrations, so every database created before it, upgraded to a build after it, failed every read of
// the runs table with "no such column". The suite's migration test dropped and re-added only the
// provenance columns, so it proved the list handled what the list handled.
//
// Open now derives the missing columns from the schema itself: whatever an existing table lacks
// relative to the CREATE statement is added, with the type and default the CREATE declares. This test
// enforces it wholesale, so a future column needs no migration entry and cannot repeat the drift.
func TestOpenHealsEveryMissingColumn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()

	// A reference database on the current schema, for the target column sets.
	refPath := filepath.Join(dir, "ref.db")
	ref, err := sqlitestore.Open(refPath)
	if err != nil {
		t.Fatalf("Open(ref): %v", err)
	}
	if err := ref.Close(); err != nil {
		t.Fatalf("Close(ref): %v", err)
	}
	refRaw, err := sql.Open("sqlite", refPath)
	if err != nil {
		t.Fatalf("open ref raw: %v", err)
	}
	defer func() { _ = refRaw.Close() }()

	// An "old" database: current schema with columns from several eras removed, on the tables that
	// have grown since the first release. Indexes over a column must go before the column can.
	oldPath := filepath.Join(dir, "old.db")
	old, err := sqlitestore.Open(oldPath)
	if err != nil {
		t.Fatalf("Open(old): %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("Close(old): %v", err)
	}
	raw, err := sql.Open("sqlite", oldPath)
	if err != nil {
		t.Fatalf("open old raw: %v", err)
	}
	for _, index := range []string{"idx_runs_actor", "idx_runs_source", "idx_runs_pending_claim"} {
		if _, err := raw.Exec("DROP INDEX IF EXISTS " + index); err != nil {
			t.Fatalf("drop %s: %v", index, err)
		}
	}
	drops := map[string][]string{
		// org_id is the column the hand list missed. The others sample every era of the table, so the
		// healer is proven on more than the one column that already burned us.
		"runs":      {"org_id", "queue", "actor_type", "policy_set", "pinned_commit", "distinct_approver"},
		"schedules": {"org_id", "timezone"},
		"templates": {"org_id", "diff_mode"},
	}
	for table, cols := range drops {
		for _, col := range cols {
			if _, err := raw.Exec(fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s", table, col)); err != nil {
				t.Fatalf("simulate old schema, drop %s.%s: %v", table, col, err)
			}
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close old raw: %v", err)
	}

	// Reopen through the store, which is the upgrade moment.
	healed, err := sqlitestore.Open(oldPath)
	if err != nil {
		t.Fatalf("reopen after downgrade: %v", err)
	}
	t.Cleanup(func() { _ = healed.Close() })

	healedRaw, err := sql.Open("sqlite", oldPath)
	if err != nil {
		t.Fatalf("open healed raw: %v", err)
	}
	defer func() { _ = healedRaw.Close() }()
	for table := range drops {
		want := tableColumns(t, refRaw, table)
		got := tableColumns(t, healedRaw, table)
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("%s columns after reopen (-fresh +healed):\n%s", table, diff)
		}
	}

	// The functional proof, which is what the demo actually died on: a run round-trips through the
	// healed database, org and all.
	store := healed.Runs()
	saved := &run.Run{
		ID: "run_1", Playbook: "site.yml", Status: run.StatusSucceeded, CreatedAt: time.Now(),
		OrgID: "org_1", Queue: "default", ActorType: "session",
	}
	if err := store.Save(ctx, saved); err != nil {
		t.Fatalf("Save after heal: %v", err)
	}
	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List after heal: %v", err)
	}
	if len(list) != 1 || list[0].OrgID != "org_1" {
		t.Errorf("List after heal = %+v, want the saved run with its org", list)
	}
}

// TestPreChainAuditTrailIsRefusedWithDirections covers the oldest databases this product ever made:
// an audit trail from the few days before the hash chain existed has no seq, and no backfill can join
// it to a chain whose hashes commit to one. The open used to die with "UNIQUE constraint failed:
// audit_entries.seq", which reads as corruption and points nowhere. It now refuses with what happened
// and what to do.
func TestPreChainAuditTrailIsRefusedWithDirections(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	// The audit table exactly as its first commit shaped it, with the rows any real database of that
	// era holds, since every mutation was recorded.
	stmts := []string{
		"CREATE TABLE audit_entries (id TEXT PRIMARY KEY, at TEXT NOT NULL, actor TEXT NOT NULL, " +
			"method TEXT NOT NULL, path TEXT NOT NULL)",
		"INSERT INTO audit_entries VALUES ('aud_1','2026-07-07T00:00:00Z','alice','POST','/v1/runs')",
		"INSERT INTO audit_entries VALUES ('aud_2','2026-07-07T00:01:00Z','alice','POST','/v1/runs')",
	}
	for _, stmt := range stmts {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("build pre-chain database: %v", err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	_, err = sqlitestore.Open(path)
	if err == nil {
		t.Fatal("a pre-chain audit trail opened cleanly, so its entries were minted into a chain " +
			"they never belonged to")
	}
	for _, want := range []string{"predates", "Archive", "2 "} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err.Error(), want)
		}
	}
}

// TestTeamMembersGainsItsPromisedForeignKey covers a constraint added to the CREATE one day after the
// table shipped, with no migration and no ALTER that could add it: a first-day database enforced
// nothing while every fresh one cascaded a deleted team's memberships. The open now rebuilds the
// table with the constraint, keeping its rows.
func TestTeamMembersGainsItsPromisedForeignKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	// Rebuild the table as its first day shaped it: same columns, no constraint, with a team and a
	// member already in it.
	stmts := []string{
		"DROP INDEX IF EXISTS idx_team_members_user",
		"DROP TABLE team_members",
		"CREATE TABLE team_members (team_id TEXT NOT NULL, user_id TEXT NOT NULL, " +
			"PRIMARY KEY (team_id, user_id))",
		"INSERT INTO teams (id, name, created_at) VALUES ('team_1','ops','2026-07-07T00:00:00Z')",
		"INSERT INTO team_members VALUES ('team_1','user_1')",
	}
	for _, stmt := range stmts {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("build first-day table: %v", err)
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
	members, err := healed.Teams().Members(ctx, "team_1")
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 1 || members[0] != "user_1" {
		t.Fatalf("members after rebuild = %v, want the stored membership kept", members)
	}
	// The constraint itself, which is the point: deleting the team cascades the membership, exactly
	// as a fresh database does.
	raw, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec("PRAGMA foreign_keys=ON"); err != nil {
		t.Fatalf("pragma: %v", err)
	}
	if _, err := raw.Exec("DELETE FROM teams WHERE id='team_1'"); err != nil {
		t.Fatalf("delete team: %v", err)
	}
	var left int
	if err := raw.QueryRow("SELECT COUNT(*) FROM team_members WHERE team_id='team_1'").Scan(&left); err != nil {
		t.Fatalf("count members: %v", err)
	}
	if left != 0 {
		t.Errorf("%d membership rows survived the team's deletion, so the rebuilt table still has "+
			"no cascade", left)
	}
}
