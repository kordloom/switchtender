package project

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/runfiles"
)

// TestASyncWaitsForAnotherProcessOnTheSameProject pins the project lock across processes. The test
// holds the project's lock file the way another process sharing the cache does while it clones or
// resets the canonical checkout, and a sync here must wait for it rather than clone and reset the
// same directory at the same time. The mutex that used to be the only lock serialized the
// goroutines of one process and nothing else.
func TestASyncWaitsForAnotherProcessOnTheSameProject(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t, map[string]string{"site.yml": "- hosts: all\n"})
	cache := t.TempDir()
	s, err := NewSyncer(cache)
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	release, err := runfiles.LockPath(filepath.Join(cache, locksSubdir, "proj_shared"))
	if err != nil {
		t.Fatalf("LockPath() error = %v", err)
	}
	type result struct {
		// wt is the worktree the sync returned.
		wt *Worktree
		// err is the sync's error.
		err error
	}
	done := make(chan result, 1)
	go func() {
		wt, err := s.Sync(&Project{ID: "proj_shared", RepoURL: repo, Branch: "main"}, "")
		done <- result{wt: wt, err: err}
	}()
	select {
	case got := <-done:
		got.wt.Cleanup()
		release()
		t.Fatalf("the sync finished (err = %v) while another process held the project's lock", got.err)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Sync() error = %v once the other process let go", got.err)
		}
		got.wt.Cleanup()
	case <-time.After(30 * time.Second):
		t.Fatal("the sync never finished after the other process let go")
	}
}

// TestSyncersSharingACacheCloneOneProjectInTurn starts several syncers on one cache at once, as
// several serve and worker processes on one host do, each cloning the same project for the first
// time. Each must get its checkout. Two clones racing into one directory failed one of them with
// "already up-to-date" before the lock reached across processes.
func TestSyncersSharingACacheCloneOneProjectInTurn(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t, map[string]string{"site.yml": "- hosts: all\n"})
	cache := t.TempDir()
	const syncers = 6
	errs := make([]error, syncers)
	var wg sync.WaitGroup
	for i := range syncers {
		s, err := NewSyncer(cache)
		if err != nil {
			t.Fatalf("NewSyncer() error = %v", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			wt, err := s.Sync(&Project{ID: "proj_together", RepoURL: repo, Branch: "main"}, "")
			if err == nil {
				if _, serr := os.Stat(filepath.Join(wt.Dir, "site.yml")); serr != nil {
					err = serr
				}
				wt.Cleanup()
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("syncer %d: %v", i, err)
		}
	}
}

// TestAStartingSyncerClearsOnlyWorktreesNobodyHolds pins what a syncer starting on a shared cache
// removes. A worktree whose process holds its lock is live and is kept, one whose lock nobody holds
// belongs to a process that died and is removed, and one with no lock file yet is being created
// this moment and is kept.
func TestAStartingSyncerClearsOnlyWorktreesNobodyHolds(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t, map[string]string{"site.yml": "- hosts: all\n"})
	cache := t.TempDir()
	first, err := NewSyncer(cache)
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	live, err := first.Sync(&Project{ID: "proj_live", RepoURL: repo, Branch: "main"}, "")
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	defer live.Cleanup()
	runs := filepath.Join(cache, runsSubdir)
	tests := []struct {
		Name     string
		Dir      string
		LockFile bool
		WantKept bool
	}{{ // Test 0: A worktree whose process died, leaving its lock file unheld.
		Name: "dead", Dir: "run-proj_dead-1", LockFile: true, WantKept: false,
	}, { // Test 1: A worktree being created, whose lock file is not written yet.
		Name: "creating", Dir: "run-proj_new-1", WantKept: true,
	}}
	for _, test := range tests {
		if err := os.MkdirAll(filepath.Join(runs, test.Dir, checkoutSubdir), 0o700); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		if test.LockFile {
			if err := os.WriteFile(filepath.Join(runs, test.Dir, ".lock"), nil, 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
		}
	}

	if _, err := NewSyncer(cache); err != nil {
		t.Fatalf("NewSyncer() second process error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(live.Dir, "site.yml")); err != nil {
		t.Errorf("the live worktree lost its playbook to the second start: %v", err)
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			_, err := os.Stat(filepath.Join(runs, test.Dir))
			if kept := err == nil; kept != test.WantKept {
				t.Errorf("worktree kept = %v (stat err = %v), want %v", kept, err, test.WantKept)
			}
		})
	}
	entries, err := os.ReadDir(runs)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	want := []string{filepath.Base(filepath.Dir(live.Dir)), "run-proj_new-1"}
	if diff := cmp.Diff(want, left, cmpopts.SortSlices(func(a, b string) bool { return a < b }),
		cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("worktrees left after the second start (-want +got):\n%s", diff)
	}
}
