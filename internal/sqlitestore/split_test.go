package sqlitestore

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// TestReadPoolIsReadOnly proves the read pool rejects writes, so a statement misrouted to it fails
// loudly instead of racing the single writer.
func TestReadPoolIsReadOnly(t *testing.T) {
	t.Parallel()
	d, err := Open(filepath.Join(t.TempDir(), "split.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = d.Close() }()
	split := d.db
	if split.r == split.w {
		t.Fatal("read pool not opened for a plain file path")
	}
	if _, err := split.r.ExecContext(context.Background(),
		"INSERT INTO orgs (id, name, created_at) VALUES ('org_x', 'x', '2026-01-01')"); err == nil {
		t.Error("write on the read pool succeeded, want a query_only failure")
	}
}

// TestReadPoolFallback proves an in-memory path keeps the single-connection behavior, since a second
// handle would open a different database.
func TestReadPoolFallback(t *testing.T) {
	t.Parallel()
	d, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = d.Close() }()
	if d.db.r != d.db.w {
		t.Error("in-memory database opened a separate read pool, want reads on the write connection")
	}
}

// TestSplitReadsSeeWrites proves reads on the pool observe rows committed on the write connection,
// and that concurrent readers and writers make progress together without errors.
func TestSplitReadsSeeWrites(t *testing.T) {
	t.Parallel()
	d, err := Open(filepath.Join(t.TempDir(), "rw.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = d.Close() }()
	ctx := context.Background()
	store := d.Runs()

	if err := store.Save(ctx, &run.Run{
		ID: "run_seed", Status: run.StatusRunning, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Get(ctx, "run_seed")
	if err != nil || got.ID != "run_seed" {
		t.Fatalf("Get() after write = %v, %v, want the committed run", got, err)
	}

	// Writers append while readers list and read logs; every operation must succeed.
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				id := fmt.Sprintf("run_%d_%d", w, i)
				if err := store.Save(ctx, &run.Run{ID: id, Status: run.StatusRunning, CreatedAt: time.Now()}); err != nil {
					errs <- fmt.Errorf("save %s: %w", id, err)
					return
				}
				if err := store.AppendLog(ctx, id, []byte("line\n")); err != nil {
					errs <- fmt.Errorf("append %s: %w", id, err)
					return
				}
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 16; i++ {
				if _, err := store.List(ctx); err != nil {
					errs <- fmt.Errorf("list: %w", err)
					return
				}
				if _, err := store.Log(ctx, "run_seed"); err != nil {
					errs <- fmt.Errorf("log: %w", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
