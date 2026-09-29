package pgstore

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/sqlutil"
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

	// Give the migration something to do. Open skips it entirely when the catalog says the schema is
	// already current, which is what stops an ordinary start from locking every table, so against an
	// up-to-date database the second Open below would take no lock at all and sail past a blocker
	// holding one. That is the improvement rather than a hole in it, but it means this check has to
	// arrange the one case that still needs the lock: a column the migration must add.
	column := anAddableColumnOn(t, "runs")
	damaged, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := damaged.ExecContext(ctx, "ALTER TABLE runs DROP COLUMN "+column+" CASCADE"); err != nil {
		_ = damaged.Close()
		t.Fatalf("drop %s to give the migration work: %v", column, err)
	}
	_ = damaged.Close()
	// The blocked migration below fails, so the column it was going to add is still missing when
	// this test ends. Every test after it reads that table, so it is put back here rather than left
	// for the next one to trip over.
	t.Cleanup(func() {
		healed, herr := Open(dsn)
		if herr != nil {
			t.Errorf("could not repair the schema this test damaged: %v", herr)
			return
		}
		_ = healed.Close()
	})

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
	// Released on the way out however this test leaves. It was released by the last statement in the
	// function instead, so any failure above that line skipped it: the transaction stayed open on a
	// pooled connection that Close cannot reclaim, the exclusive lock on runs was never dropped, and
	// every later test in the package blocked on it until the whole run timed out.
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "LOCK TABLE runs IN ACCESS EXCLUSIVE MODE"); err != nil {
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
}

// anAddableColumnOn returns a column on the named table that the migration's ALTER statements can
// add, which is what makes dropping it something the next Open has to repair.
func anAddableColumnOn(t *testing.T, table string) string {
	t.Helper()
	for _, col := range sqlutil.ParseSchemaColumns(schema)[table] {
		if col.Addable() {
			return col.Name
		}
	}
	t.Fatalf("the schema declares no column on %s that an ALTER can add, so the migration cannot "+
		"be given work to do and this check cannot run", table)
	return ""
}
