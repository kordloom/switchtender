//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows

package runfiles

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestRemoveDeadLeavesEveryLiveDirectory pins the one-observation removal against each shape a run
// directory takes. Only a directory whose owner is provably gone may go: its lock file is there and
// nobody holds it, or it never got a lock file and has sat unchanged past MinAge.
func TestRemoveDeadLeavesEveryLiveDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	live, err := Create(root, "live")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer func() { _ = live.Remove() }()
	tests := []struct {
		Name     string
		Dir      string
		LockFile bool
		Age      time.Duration
		WantKept bool
	}{{ // Test 0: The owner died and the operating system released its lock.
		Name: "lock free", Dir: "run-dead-1", LockFile: true, WantKept: false,
	}, { // Test 1: No lock file yet, so it is being created this moment.
		Name: "creating", Dir: "run-new-1", WantKept: true,
	}, { // Test 2: No lock file and unchanged past MinAge, so its creator died creating it.
		Name: "abandoned while created", Dir: "run-old-1", Age: MinAge + time.Minute, WantKept: false,
	}, { // Test 3: Not a run directory at all.
		Name: "not a run directory", Dir: "other", LockFile: true, WantKept: true,
	}}
	for _, test := range tests {
		dir := filepath.Join(root, test.Dir)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("Mkdir() error = %v", err)
		}
		if test.LockFile {
			if err := os.WriteFile(filepath.Join(dir, lockName), nil, 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
		}
		if test.Age > 0 {
			then := time.Now().Add(-test.Age)
			if err := os.Chtimes(dir, then, then); err != nil {
				t.Fatalf("Chtimes() error = %v", err)
			}
		}
	}

	removed, err := RemoveDead(root)
	if err != nil {
		t.Fatalf("RemoveDead() error = %v", err)
	}
	if removed != 2 {
		t.Errorf("RemoveDead() removed %d, want 2", removed)
	}
	if _, err := os.Stat(live.Path()); err != nil {
		t.Errorf("the directory a live owner holds was removed: %v", err)
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			_, err := os.Stat(filepath.Join(root, test.Dir))
			if kept := err == nil; kept != test.WantKept {
				t.Errorf("kept = %v (stat err = %v), want %v", kept, err, test.WantKept)
			}
		})
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	sort.Strings(left)
	want := []string{"other", filepath.Base(live.Path()), "run-new-1"}
	sort.Strings(want)
	if diff := cmp.Diff(want, left, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("left under the root (-want +got):\n%s", diff)
	}
	if n, err := RemoveDead(filepath.Join(root, "missing")); err != nil || n != 0 {
		t.Errorf("RemoveDead() on a missing root = %d, %v, want 0, nil", n, err)
	}
}

// TestLockPathExcludesASecondHolderUntilReleased pins the lock a process takes on a shared file: a
// second holder, which a flock treats the same in one process as in another, waits until the first
// lets go.
func TestLockPathExcludesASecondHolderUntilReleased(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "project.lock")
	release, err := LockPath(path)
	if err != nil {
		t.Fatalf("LockPath() error = %v", err)
	}
	got := make(chan func(), 1)
	go func() {
		second, err := LockPath(path)
		if err != nil {
			t.Errorf("second LockPath() error = %v", err)
			got <- func() {}
			return
		}
		got <- second
	}()
	select {
	case second := <-got:
		second()
		release()
		t.Fatal("a second holder took the lock while the first still held it")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case second := <-got:
		second()
	case <-time.After(5 * time.Second):
		t.Fatal("the second holder never took the lock after the first let go")
	}
}
