//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package runfiles

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestRemoveFinishesBeforeTheLockIsReleased stalls a Create between making its lock file and
// locking it while a sweep that found the lock free removes the directory, and resumes the Create
// the moment the sweep releases the lock. The Create takes the lock then, so had its lock file
// still been at its path it would have kept the directory, and the sweep would have deleted it from
// under the run a moment later. The directory and its lock file must be gone before the release.
func TestRemoveFinishesBeforeTheLockIsReleased(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name  string
		Files []string
	}{{ // Test 0: The Create stalled before writing anything but its lock file.
		Name: "only the lock file",
	}, { // Test 1: The directory also holds what a run writes, which goes under the lock as well.
		Name: "run material", Files: []string{beatName, "credential-1"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "run-stalled-1")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatalf("Mkdir() error = %v", err)
			}
			stalled, err := createLockFile(filepath.Join(dir, lockName))
			if err != nil {
				t.Fatalf("createLockFile() error = %v", err)
			}
			defer func() { _ = stalled.Close() }()
			for _, name := range test.Files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("1"), 0o600); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}
			}
			sweep, err := openLockFile(filepath.Join(dir, lockName))
			if err != nil {
				t.Fatalf("openLockFile() error = %v", err)
			}
			if free, err := lockFile(sweep, false); err != nil || !free {
				_ = sweep.Close()
				t.Fatalf("lockFile() = %v, %v, want the sweep to find the lock free", free, err)
			}
			var locked, kept bool
			err = removeHeld(dir, func() {
				_ = unlockFile(sweep)
				_ = sweep.Close()
				// The stalled Create resumes here, the moment the lock is free, and does what
				// Create does next: lock, then check the lock file is still the one at its path.
				locked, _ = lockFile(stalled, false)
				kept = locked && stillThere(stalled, dir)
			})
			if err != nil {
				t.Fatalf("removeHeld() error = %v", err)
			}
			if !locked {
				t.Fatal("the stalled Create could not take the lock the sweep released")
			}
			if kept {
				t.Error("the stalled Create found its lock file in place and kept a directory the " +
					"sweep deleted after releasing the lock")
			}
			if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the directory survived the removal: %v", err)
			}
		})
	}
}
