package pgstore

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMigrateFailsFastRatherThanQueueingBehindALock proves the migration gives up on a lock instead
// of waiting for it.
//
// An ADD COLUMN IF NOT EXISTS that changes nothing still takes AccessExclusiveLock, and the
// migration issues one per declared column inside a single transaction, so it ends up holding an
// exclusive lock on every table until it commits. Every process calls Open, workers included, so it
// runs on ordinary starts. With no timeout a node coming up behind one long read queued for the
// lock, and PostgreSQL's lock queue is first in first out, so every later reader queued behind the
// migration: one slow retention purge could stall the whole cluster for as long as it ran.
func TestMigrateFailsFastRatherThanQueueingBehindALock(t *testing.T) {
	dsn := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if dsn == "" {
		// The release gate demands the full suite, in which a missing database is a failure
		// rather than a quiet green.
		if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
			t.Fatal("SWITCHTENDER_REQUIRE_FULL_SUITE is set and SWITCHTENDER_TEST_POSTGRES_DSN " +
				"is not: the full suite was demanded and this migration check cannot run")
		}
		t.Skip("SWITCHTENDER_TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()

	// One migration first, so the tables exist and the second one needs locks on them.
	first, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })

	// A separate session holds an exclusive lock on runs, standing in for the long read.
	blocker, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer func() { _ = blocker.Close() }()
	tx, err := blocker.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	if _, err := tx.ExecContext(ctx, "LOCK TABLE runs IN ACCESS EXCLUSIVE MODE"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("LOCK TABLE error = %v", err)
	}

	// A second Open must give up rather than hang, and must do so well inside the lock timeout plus
	// a margin, not after the blocker eventually releases.
	done := make(chan error, 1)
	go func() {
		db, err := Open(dsn)
		if db != nil {
			_ = db.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the migration acquired the lock while another session held it exclusively")
		}
		if !strings.Contains(err.Error(), "lock timeout") && !strings.Contains(err.Error(), "canceling") {
			t.Logf("migration failed with: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Error("the migration is still waiting for the lock, so a starting node stalls every " +
			"reader queued behind it")
	}
	_ = tx.Rollback()
}
