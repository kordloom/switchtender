package sqlitestore_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlitestore"
	"github.com/kordloom/switchtender/internal/sqlutil"
	"github.com/kordloom/switchtender/internal/storetest"
)

// TestLongNamesStoredWholeStayFound pins that a long host or task name an earlier release stored
// whole is left as it is and is still found by that name. SQLite has no limit on an index entry, so
// such rows exist, and a lookup that matched only the cut form would hide their history.
func TestLongNamesStoredWholeStayFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "switchtender.db")
	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	at := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if err := db.Runs().Save(ctx, &run.Run{
		ID: "rwhole", Playbook: "site.yml", Status: run.StatusSucceeded, CreatedAt: at,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	host := strings.Repeat("h", 3*run.MaxSummaryNameBytes)
	task := strings.Repeat("t", 3*run.MaxSummaryNameBytes)
	when := sqlutil.FormatTime(at)
	for _, stmt := range []struct {
		Query string
		Args  []any
	}{{
		Query: `INSERT INTO run_host_summary (run_id, host, ok, changed, failures, unreachable,
	skipped, worst, duration_seconds, ran_at, dry_run) VALUES (?, ?, 1, 0, 0, 0, 0, 'ok', 1.5, ?, 0)`,
		Args: []any{"rwhole", host, when},
	}, {
		Query: "INSERT INTO run_task_summary (run_id, task, seconds, ran_at) VALUES (?, ?, 1.5, ?)",
		Args:  []any{"rwhole", task, when},
	}, {
		Query: "INSERT INTO host_facts (host, run_id, facts, gathered_at) VALUES (?, ?, ?, ?)",
		Args:  []any{host, "rwhole", `{"distro":"debian"}`, when},
	}} {
		if _, err := raw.ExecContext(ctx, stmt.Query, stmt.Args...); err != nil {
			t.Fatalf("write a row the way an earlier release did: %v", err)
		}
	}
	storetest.LongNamesStoredWhole(t, db.Runs(), "rwhole", host, task)
}
