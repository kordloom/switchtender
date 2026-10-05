package project_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/project"
)

// prOrigin builds a repository whose main branch holds one commit and whose refs/pull/7/head points
// at a second commit no branch holds, the shape a forge gives a pull request. It returns the
// repository path and both commit hashes.
func prOrigin(t *testing.T) (dir, mainSHA, prSHA string) {
	t.Helper()
	dir = t.TempDir()
	repo, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.Main},
	})
	if err != nil {
		t.Fatalf("PlainInitWithOptions() error = %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree() error = %v", err)
	}
	commit := func(msg string, files map[string]string, mode os.FileMode) plumbing.Hash {
		for rel, body := range files {
			full := filepath.Join(dir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
			if err := os.WriteFile(full, []byte(body), mode); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			if err := os.Chmod(full, mode); err != nil {
				t.Fatalf("Chmod() error = %v", err)
			}
		}
		if err := wt.AddGlob("."); err != nil {
			t.Fatalf("AddGlob() error = %v", err)
		}
		h, err := wt.Commit(msg, &git.CommitOptions{
			Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now()},
		})
		if err != nil {
			t.Fatalf("Commit() error = %v", err)
		}
		return h
	}
	first := commit("first", map[string]string{"infra/main.tf": "# main\n"}, 0o644)
	if err := wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("feature"), Create: true,
	}); err != nil {
		t.Fatalf("Checkout(feature) error = %v", err)
	}
	commit("proposed", map[string]string{"infra/main.tf": "# proposed\n"}, 0o644)
	pr := commit("script", map[string]string{"bin/plan.sh": "#!/bin/sh\n"}, 0o755)
	if err := repo.Storer.SetReference(plumbing.NewHashReference("refs/pull/7/head", pr)); err != nil {
		t.Fatalf("SetReference() error = %v", err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{Branch: plumbing.Main}); err != nil {
		t.Fatalf("Checkout(main) error = %v", err)
	}
	if err := repo.Storer.RemoveReference(plumbing.NewBranchReferenceName("feature")); err != nil {
		t.Fatalf("RemoveReference() error = %v", err)
	}
	return dir, first.String(), pr.String()
}

// TestSyncRefChecksOutAPullRequestHead proves a ref sync runs the commit the ref names, which no
// branch holds, and leaves the branch checkout where it was.
//
// A review plan has to execute the pull request's commit. The branch sync can only ever reach the
// branch, so without a ref sync the plan ran the base branch and reported on code the pull request
// did not contain.
func TestSyncRefChecksOutAPullRequestHead(t *testing.T) {
	t.Parallel()
	origin, mainSHA, prSHA := prOrigin(t)
	s, err := project.NewSyncer(t.TempDir())
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	p := &project.Project{ID: "proj_pr", RepoURL: origin, Branch: "main"}

	wt, err := s.SyncRef(p, "", "refs/pull/7/head")
	if err != nil {
		t.Fatalf("SyncRef() error = %v", err)
	}
	defer wt.Cleanup()
	if diff := cmp.Diff(prSHA, wt.SHA); diff != "" {
		t.Errorf("SyncRef() SHA mismatch (-want +got):\n%s", diff)
	}
	body, err := os.ReadFile(filepath.Join(wt.Dir, "infra", "main.tf"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if diff := cmp.Diff("# proposed\n", string(body)); diff != "" {
		t.Errorf("checked out content mismatch (-want +got):\n%s", diff)
	}
	info, err := os.Stat(filepath.Join(wt.Dir, "bin", "plan.sh"))
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("plan.sh mode = %v, want the executable bit kept", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(wt.Dir, ".git")); !os.IsNotExist(err) {
		t.Errorf(".git was written into the run checkout, err = %v", err)
	}

	branch, err := s.Sync(p, "")
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	defer branch.Cleanup()
	if diff := cmp.Diff(mainSHA, branch.SHA); diff != "" {
		t.Errorf("the branch sync after a ref sync moved (-want +got):\n%s", diff)
	}
}

// TestSyncRefRefusesBadRefs pins the refs a run may not ask for: anything that is not a full
// reference, anything shaped like a refspec or an option, and a ref the remote does not have.
func TestSyncRefRefusesBadRefs(t *testing.T) {
	t.Parallel()
	origin, _, _ := prOrigin(t)
	s, err := project.NewSyncer(t.TempDir())
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	p := &project.Project{ID: "proj_bad", RepoURL: origin, Branch: "main"}
	tests := []struct {
		Ref        string
		WantBadRef bool
	}{{ // Test 0: A short branch name is not a full reference.
		Ref: "main", WantBadRef: true,
	}, { // Test 1: A refspec that writes a local branch is refused.
		Ref: "refs/pull/7/head:refs/heads/main", WantBadRef: true,
	}, { // Test 2: A leading dash is refused.
		Ref: "-refs/pull/7/head", WantBadRef: true,
	}, { // Test 3: A dot dot component is refused.
		Ref: "refs/pull/../head", WantBadRef: true,
	}, { // Test 4: A forced refspec is refused.
		Ref: "+refs/pull/7/head", WantBadRef: true,
	}, { // Test 5: A well-formed ref the remote does not hold fails the fetch.
		Ref: "refs/pull/99/head",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			wt, err := s.SyncRef(p, "", test.Ref)
			if err == nil {
				wt.Cleanup()
				t.Fatalf("SyncRef(%q) error = nil, want a refusal", test.Ref)
			}
			if got := errors.Is(err, project.ErrBadRef); got != test.WantBadRef {
				t.Errorf("SyncRef(%q) error = %v, ErrBadRef = %v, want %v", test.Ref, err, got,
					test.WantBadRef)
			}
		})
	}
}
