package pgstore

import (
	"context"
	"os"
	"testing"

	"github.com/kordloom/switchtender/internal/user"
)

// snapOpen opens a store on a PostgreSQL database of this test's own, plus a plain handle on the
// same database for what the test does around the store.
func snapOpen(t *testing.T) (*DB, string) {
	t.Helper()
	shared := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if shared == "" {
		skipOrFail(t, "SWITCHTENDER_TEST_POSTGRES_DSN not set")
	}
	dsn := freshDatabase(t, shared)
	db, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, dsn
}

// TestAReadSnapshotThatDidNotHoldFailsItsRelease kills the connection holding a backup's snapshot
// partway through the reads, the way a server restart, a pooler, or a network fault ends one. The
// pool opens a fresh connection for the next read, outside the snapshot, and nothing on the read
// path notices. The release has to: a backup that sealed reads from two instants as one would be
// the torn copy the snapshot exists to prevent.
func TestAReadSnapshotThatDidNotHoldFailsItsRelease(t *testing.T) {
	t.Parallel()
	db, dsn := snapOpen(t)
	ctx := context.Background()
	release, err := db.BeginReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("BeginReadSnapshot() error = %v", err)
	}
	var backend int64
	if err := db.db.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&backend); err != nil {
		t.Fatalf("read the snapshot's backend: %v", err)
	}
	raw := rawHandle(t, dsn)
	if _, err := raw.ExecContext(ctx, "SELECT pg_terminate_backend($1)", backend); err != nil {
		t.Fatalf("end the snapshot's connection: %v", err)
	}
	// The reads go on, on whatever connection the pool hands them.
	_, _ = db.Users().List(ctx)
	_, _ = db.Users().List(ctx)
	if err := release(); err == nil {
		t.Error("the release of a snapshot whose connection was replaced mid-read = nil, want an " +
			"error: the backup would seal reads taken at two instants as one")
	}

	// The pool is whole again afterward, outside any snapshot.
	u, err := user.New("after-release", "correct-horse-battery", user.RoleOperator)
	if err != nil {
		t.Fatalf("user.New: %v", err)
	}
	if err := db.Users().Save(ctx, u); err != nil {
		t.Errorf("a write after the release error = %v, want the pool out of the snapshot", err)
	}
}

// TestAReadSnapshotLeavesThePoolWritableAfterward takes a snapshot, reads through it, releases it,
// and then writes, so a snapshot that left its read-only transaction or its single connection
// behind would fail the write.
func TestAReadSnapshotLeavesThePoolWritableAfterward(t *testing.T) {
	t.Parallel()
	db, _ := snapOpen(t)
	ctx := context.Background()
	release, err := db.BeginReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("BeginReadSnapshot() error = %v", err)
	}
	if _, err := db.Users().List(ctx); err != nil {
		t.Fatalf("List() inside the snapshot error = %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release() error = %v", err)
	}
	u, err := user.New("after-snapshot", "correct-horse-battery", user.RoleOperator)
	if err != nil {
		t.Fatalf("user.New: %v", err)
	}
	if err := db.Users().Save(ctx, u); err != nil {
		t.Errorf("Save() after the snapshot error = %v, want the pool writable again", err)
	}
	if got := db.db.Stats().MaxOpenConnections; got != poolMaxOpen {
		t.Errorf("the pool allows %d connections after the snapshot, want %d", got, poolMaxOpen)
	}
}
