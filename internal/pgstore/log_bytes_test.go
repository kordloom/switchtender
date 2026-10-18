package pgstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// logBytesOf reads a run's running log byte count straight from the database.
func logBytesOf(t *testing.T, raw *sql.DB, id string) int64 {
	t.Helper()
	var n int64
	if err := raw.QueryRow("SELECT log_bytes FROM runs WHERE id=$1", id).Scan(&n); err != nil {
		t.Fatalf("read log_bytes of %s: %v", id, err)
	}
	return n
}

// TestAppendLogKeepsARunningByteCount pins that the cap is fenced on a count the database keeps.
// A chunk inserted the way a release that predates the count inserts it, plainly into run_logs,
// is counted too, since a rolling update and a rollback both leave such a release appending.
func TestAppendLogKeepsARunningByteCount(t *testing.T) {
	shared := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if shared == "" {
		skipOrFail(t, "SWITCHTENDER_TEST_POSTGRES_DSN not set")
	}
	own := freshDatabase(t, shared)
	db, err := Open(own)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	raw := rawHandle(t, own)
	ctx := context.Background()
	store := db.Runs()
	if err := store.Save(ctx, &run.Run{ID: "run_count", Status: run.StatusRunning,
		CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	for _, chunk := range []string{"alpha\n", "bravo bravo\n", "c"} {
		if err := store.AppendLog(ctx, "run_count", []byte(chunk)); err != nil {
			t.Fatalf("AppendLog(%q) error = %v", chunk, err)
		}
	}
	want := int64(len("alpha\n") + len("bravo bravo\n") + len("c"))
	if got := logBytesOf(t, raw, "run_count"); got != want {
		t.Errorf("log_bytes after three appends = %d, want %d", got, want)
	}
	if _, err := raw.Exec(
		"INSERT INTO run_logs (run_id, chunk) VALUES ('run_count', '\\x0102030405')"); err != nil {
		t.Fatalf("plain insert: %v", err)
	}
	want += 5
	if got := logBytesOf(t, raw, "run_count"); got != want {
		t.Errorf("log_bytes after a plain insert from an earlier release = %d, want %d", got, want)
	}
}

// TestRacingAppendersCannotBothCrossTheCap pins that writers on separate connections serialize on
// the run's count: with the run one byte under the cap, exactly one of many concurrent appends is
// stored, where a fence that read the count without taking the row would let several through.
func TestRacingAppendersCannotBothCrossTheCap(t *testing.T) {
	shared := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if shared == "" {
		skipOrFail(t, "SWITCHTENDER_TEST_POSTGRES_DSN not set")
	}
	own := freshDatabase(t, shared)
	db, err := Open(own)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	raw := rawHandle(t, own)
	ctx := context.Background()
	store := db.Runs()
	// One round is a short window, so the race is run over many runs, each of them one byte under the
	// cap with every writer released at once.
	const writers, rounds = 16, 60
	for round := 0; round < rounds; round++ {
		id := fmt.Sprintf("run_race_%d", round)
		if err := store.Save(ctx, &run.Run{ID: id, Status: run.StatusRunning,
			CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		if _, err := raw.Exec("UPDATE runs SET log_bytes = $1 WHERE id=$2", run.MaxLogBytes-1, id); err != nil {
			t.Fatalf("set the count: %v", err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if err := store.AppendLog(ctx, id, []byte("x")); err != nil {
					t.Errorf("AppendLog() error = %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()
		var stored int
		if err := raw.QueryRow("SELECT COUNT(*) FROM run_logs WHERE run_id=$1", id).Scan(&stored); err != nil {
			t.Fatalf("count chunks: %v", err)
		}
		if stored != 1 {
			t.Fatalf("round %d: stored chunks = %d, want exactly 1 across %d racing writers",
				round, stored, writers)
		}
	}
}

// TestUpgradeBackfillsTheByteCountOfRunsInFlight opens a database made before runs counted their
// log bytes. A run in flight gets its count from the chunks it holds, a finished run is left
// alone, and a second open recounts nothing.
func TestUpgradeBackfillsTheByteCountOfRunsInFlight(t *testing.T) {
	shared := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if shared == "" {
		skipOrFail(t, "SWITCHTENDER_TEST_POSTGRES_DSN not set")
	}
	own := freshDatabase(t, shared)
	first, err := Open(own)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	for _, r := range []*run.Run{
		{ID: "run_live", Status: run.StatusRunning, CreatedAt: now},
		{ID: "run_done", Status: run.StatusSucceeded, CreatedAt: now},
	} {
		if err := first.Runs().Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	raw := rawHandle(t, own)
	for _, stmt := range []string{
		"DROP TRIGGER run_logs_count ON run_logs",
		"ALTER TABLE runs DROP COLUMN log_bytes",
		"INSERT INTO run_logs (run_id, chunk) VALUES ('run_live', '\\x01020304')",
		"INSERT INTO run_logs (run_id, chunk) VALUES ('run_live', '\\x0506')",
		"INSERT INTO run_logs (run_id, chunk) VALUES ('run_done', '\\x0708090a0b')",
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("simulate the old schema, %s: %v", stmt, err)
		}
	}
	upgraded, err := Open(own)
	if err != nil {
		t.Fatalf("reopen the old database: %v", err)
	}
	t.Cleanup(func() { _ = upgraded.Close() })
	if got := logBytesOf(t, raw, "run_live"); got != 6 {
		t.Errorf("log_bytes of the run in flight after the upgrade = %d, want 6", got)
	}
	if got := logBytesOf(t, raw, "run_done"); got != 0 {
		t.Errorf("log_bytes of the finished run after the upgrade = %d, want 0", got)
	}
	if err := upgraded.Runs().AppendLog(ctx, "run_live", []byte("xyz")); err != nil {
		t.Fatalf("AppendLog() after the upgrade error = %v", err)
	}
	if got := logBytesOf(t, raw, "run_live"); got != 9 {
		t.Errorf("log_bytes after an append on the upgraded database = %d, want 9", got)
	}
}
