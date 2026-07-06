package sqlitestore_test

import (
	"context"
	"database/sql"
	"github.com/dcadolph/yardmaster/internal/auth"
	"github.com/dcadolph/yardmaster/internal/authtest"
	"github.com/dcadolph/yardmaster/internal/credential"
	"github.com/dcadolph/yardmaster/internal/credtest"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/dcadolph/yardmaster/internal/run"
	"github.com/dcadolph/yardmaster/internal/schedule"
	"github.com/dcadolph/yardmaster/internal/scheduletest"
	"github.com/dcadolph/yardmaster/internal/sqlitestore"
	"github.com/dcadolph/yardmaster/internal/storetest"
)

func TestStoreContract(t *testing.T) {
	t.Parallel()
	storetest.Contract(t, func() run.Store {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "yardmaster.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.Runs()
	})
}

func TestOpenAddsColumnsToOlderDatabase(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "yardmaster.db")

	// Lay down tables that predate the retry_of and duration_seconds columns.
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	const oldSchema = `
CREATE TABLE runs (
	id            TEXT PRIMARY KEY,
	playbook      TEXT NOT NULL,
	inventory     TEXT NOT NULL,
	status        TEXT NOT NULL,
	exit_code     INTEGER,
	error         TEXT NOT NULL DEFAULT '',
	created_at    TEXT NOT NULL,
	started_at    TEXT,
	ended_at      TEXT,
	parent_id     TEXT,
	shard_index   INTEGER,
	shard_count   INTEGER,
	limit_pattern TEXT NOT NULL DEFAULT '',
	kind          TEXT NOT NULL DEFAULT '',
	step_name     TEXT NOT NULL DEFAULT '',
	step_index    INTEGER
);
CREATE TABLE run_host_summary (
	run_id      TEXT NOT NULL,
	host        TEXT NOT NULL,
	ok          INTEGER NOT NULL,
	changed     INTEGER NOT NULL,
	failures    INTEGER NOT NULL,
	unreachable INTEGER NOT NULL,
	skipped     INTEGER NOT NULL,
	worst       TEXT NOT NULL,
	ran_at      TEXT NOT NULL,
	PRIMARY KEY (run_id, host)
);
INSERT INTO runs (id, playbook, inventory, status, created_at)
VALUES ('run_old', 'play.yml', 'inv', 'succeeded', '2026-01-02T03:04:05Z');`
	if _, err := old.Exec(oldSchema); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close raw sqlite: %v", err)
	}

	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := db.Runs()
	ctx := context.Background()

	got, err := store.Get(ctx, "run_old")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.RetryOf != nil {
		t.Errorf("RetryOf = %v, want nil on a pre-migration row", got.RetryOf)
	}

	retryOf := "run_old"
	if err := store.Save(ctx, &run.Run{
		ID: "run_new", Playbook: "play.yml", Status: run.StatusPending,
		CreatedAt: time.Now(), RetryOf: &retryOf,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.SaveHostSummary(ctx, "run_new", []run.HostSummary{
		{Host: "web01", Worst: "ok", DurationSeconds: 2.5, RanAt: time.Now()},
	}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	costs, err := store.HostCosts(ctx, 5)
	if err != nil {
		t.Fatalf("HostCosts() error = %v", err)
	}
	if costs["web01"] != 2.5 {
		t.Errorf("web01 cost = %v, want 2.5", costs["web01"])
	}
}

func TestScheduleStoreContract(t *testing.T) {
	t.Parallel()
	scheduletest.Contract(t, func() schedule.Store {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "yardmaster.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.Schedules()
	})
}

func TestTokenStoreContract(t *testing.T) {
	t.Parallel()
	authtest.Contract(t, func() auth.Store {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "yardmaster.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.Tokens()
	})
}

func TestCredentialStoreContract(t *testing.T) {
	t.Parallel()
	credtest.Contract(t, func() credential.Store {
		db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "yardmaster.db"))
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db.Credentials()
	})
}
