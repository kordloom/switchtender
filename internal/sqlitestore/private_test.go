//go:build unix

package sqlitestore

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestANewDatabaseIsReadableByItsOwnerAlone pins the permissions a fresh database is created with.
//
// It held hashed tokens, sealed credentials, stored inventory content, and the audit chain, and it
// was created with the process umask, which on a stock system let every local account read it. The
// WAL and shared-memory files SQLite creates beside it take the database file's permissions, so they
// are checked too, after a write that brings them into being.
func TestANewDatabaseIsReadableByItsOwnerAlone(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	path := filepath.Join(t.TempDir(), "switchtender.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Runs().Save(context.Background(), &run.Run{ID: "run_perm", Status: run.StatusPending}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatalf("Stat(%s) error = %v", filepath.Base(name), err)
		}
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("%s is %v, want it readable by its owner alone", filepath.Base(name), mode)
		}
	}
}
