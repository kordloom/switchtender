package sqlitestore_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// loggedBytes reads a run's running log byte count straight from the database file.
func loggedBytes(t *testing.T, path, id string) int64 {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw %s: %v", path, err)
	}
	defer func() { _ = raw.Close() }()
	var n int64
	if err := raw.QueryRow("SELECT log_bytes FROM runs WHERE id=?", id).Scan(&n); err != nil {
		t.Fatalf("read log_bytes of %s: %v", id, err)
	}
	return n
}

// TestAppendLogKeepsARunningByteCount pins that the cap is fenced on a count the database keeps,
// not on a sum over every earlier chunk. A chunk written by a release that predates the count, a
// plain insert into run_logs, must still be counted, since a rolling update and a rollback both
// leave such a release appending to the same database.
func TestAppendLogKeepsARunningByteCount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "switchtender.db")
	db := openStoreAt(t, path)
	store := db.Runs()
	saveRuns(t, store, &run.Run{ID: "run_count", Status: run.StatusRunning, CreatedAt: baseTime})

	for _, chunk := range []string{"alpha\n", "bravo bravo\n", "c"} {
		if err := store.AppendLog(ctx, "run_count", []byte(chunk)); err != nil {
			t.Fatalf("AppendLog(%q) error = %v", chunk, err)
		}
	}
	want := int64(len("alpha\n") + len("bravo bravo\n") + len("c"))
	if got := loggedBytes(t, path, "run_count"); got != want {
		t.Errorf("log_bytes after three appends = %d, want %d", got, want)
	}

	rawExec(t, path, "INSERT INTO run_logs (run_id, chunk) VALUES ('run_count', x'0102030405')")
	want += 5
	if got := loggedBytes(t, path, "run_count"); got != want {
		t.Errorf("log_bytes after a plain insert from an earlier release = %d, want %d", got, want)
	}
}

// TestAppendLogFencesOnTheCount pins that the refusal reads the count and not the chunks: a run
// whose count sits at the cap refuses the next chunk and says so, and one byte under the cap
// accepts exactly one more chunk. The count is set directly so the test writes no 64 MiB.
func TestAppendLogFencesOnTheCount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "switchtender.db")
	db := openStoreAt(t, path)
	store := db.Runs()
	saveRuns(t, store, &run.Run{ID: "run_fence", Status: run.StatusRunning, CreatedAt: baseTime})
	rawExec(t, path, fmt.Sprintf("UPDATE runs SET log_bytes = %d WHERE id = 'run_fence'", run.MaxLogBytes-1))

	if err := store.AppendLog(ctx, "run_fence", []byte("last chunk crosses the cap")); err != nil {
		t.Fatalf("AppendLog() one byte under the cap error = %v", err)
	}
	if err := store.AppendLog(ctx, "run_fence", []byte("refused")); err != nil {
		t.Fatalf("AppendLog() at the cap error = %v, want a silent refusal", err)
	}
	got, err := store.Log(ctx, "run_fence")
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	if string(got) != "last chunk crosses the cap" {
		t.Errorf("Log() = %q, want only the chunk that crossed the cap", got)
	}
	r, err := store.Get(ctx, "run_fence")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if r.Warning != run.LogTruncatedWarning {
		t.Errorf("Warning = %q, want %q", r.Warning, run.LogTruncatedWarning)
	}
}

// TestUpgradeBackfillsTheByteCountOfRunsInFlight opens a database made before runs counted their
// log bytes. A run still in flight gets its count from the chunks it already holds, and a finished
// run is left alone, since it never appends again and counting every old log would make the
// upgrade cost the whole history. Chunks appended afterward are counted either way.
func TestUpgradeBackfillsTheByteCountOfRunsInFlight(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	first, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	saveRuns(t, first.Runs(),
		&run.Run{ID: "run_live", Status: run.StatusRunning, CreatedAt: baseTime},
		&run.Run{ID: "run_done", Status: run.StatusSucceeded, CreatedAt: baseTime.Add(time.Second)})
	if err := first.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	rawExec(t, path,
		"DROP TRIGGER run_logs_count",
		"ALTER TABLE runs DROP COLUMN log_bytes",
		"INSERT INTO run_logs (run_id, chunk) VALUES ('run_live', x'01020304')",
		"INSERT INTO run_logs (run_id, chunk) VALUES ('run_live', x'0506')",
		"INSERT INTO run_logs (run_id, chunk) VALUES ('run_done', x'0708090a0b')")

	upgraded := openStoreAt(t, path)
	if got := loggedBytes(t, path, "run_live"); got != 6 {
		t.Errorf("log_bytes of the run in flight after the upgrade = %d, want 6", got)
	}
	if got := loggedBytes(t, path, "run_done"); got != 0 {
		t.Errorf("log_bytes of the finished run after the upgrade = %d, want 0", got)
	}
	if err := upgraded.Runs().AppendLog(ctx, "run_live", []byte("xyz")); err != nil {
		t.Fatalf("AppendLog() after the upgrade error = %v", err)
	}
	if got := loggedBytes(t, path, "run_live"); got != 9 {
		t.Errorf("log_bytes after an append on the upgraded database = %d, want 9", got)
	}
}

// TestReopenDoesNotRecountTheLog pins that the backfill runs once, with the trigger, and not on
// every start: a count the trigger has kept is left as it is.
func TestReopenDoesNotRecountTheLog(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "switchtender.db")
	first, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	saveRuns(t, first.Runs(), &run.Run{ID: "run_stay", Status: run.StatusRunning, CreatedAt: baseTime})
	if err := first.Runs().AppendLog(ctx, "run_stay", []byte("abcd")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	rawExec(t, path, "UPDATE runs SET log_bytes = 999 WHERE id = 'run_stay'")
	openStoreAt(t, path)
	if got := loggedBytes(t, path, "run_stay"); got != 999 {
		t.Errorf("log_bytes after a reopen = %d, want the kept 999: the start recounted the log", got)
	}
}

// BenchmarkAppendLogByLogLength measures one append against a log already holding n chunks. The
// cost should not grow with n.
func BenchmarkAppendLogByLogLength(b *testing.B) {
	for _, n := range []int{1_000, 50_000} {
		b.Run(fmt.Sprintf("chunks=%d", n), func(b *testing.B) {
			ctx := context.Background()
			db, err := sqlitestore.Open(filepath.Join(b.TempDir(), "switchtender.db"))
			if err != nil {
				b.Fatalf("Open() error = %v", err)
			}
			b.Cleanup(func() { _ = db.Close() })
			store := db.Runs()
			if err := store.Save(ctx, &run.Run{ID: "run_bench", Status: run.StatusRunning,
				CreatedAt: baseTime}); err != nil {
				b.Fatalf("Save() error = %v", err)
			}
			chunk := []byte("TASK [ping] ok: [host]\n")
			for i := 0; i < n; i++ {
				if err := store.AppendLog(ctx, "run_bench", chunk); err != nil {
					b.Fatalf("AppendLog() error = %v", err)
				}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := store.AppendLog(ctx, "run_bench", chunk); err != nil {
					b.Fatalf("AppendLog() error = %v", err)
				}
			}
		})
	}
}
