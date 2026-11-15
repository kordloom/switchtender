package project

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// commitLocal writes files into the working copy at dir and commits them on main, creating the
// repository first when there is none, and returns the commit.
func commitLocal(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		repo, err = git.PlainInitWithOptions(dir, &git.PlainInitOptions{
			InitOptions: git.InitOptions{DefaultBranch: plumbing.Main},
		})
	}
	if err != nil {
		t.Fatalf("open repository %s: %v", dir, err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree() error = %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob() error = %v", err)
	}
	head, err := wt.Commit("change", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	return head.String()
}

// TestALocalProjectSyncsWithNoGitBinary syncs a project whose remote is a folder on this server
// with PATH emptied, the way the release image has no git. go-git's own file transport runs
// git-upload-pack as a program, so this failed wherever no git was installed. The in-process
// transport reads the repository itself, for a working copy, a bare repository, and a file URL,
// and a fetch after a new commit moves the checkout to it.
//
// Not parallel: it empties PATH for the process.
func TestALocalProjectSyncsWithNoGitBinary(t *testing.T) {
	work := t.TempDir()
	first := commitLocal(t, work, map[string]string{"site.yml": "- hosts: all\n  tasks: []\n"})
	bare := filepath.Join(t.TempDir(), "infra.git")
	if _, err := git.PlainClone(bare, true, &git.CloneOptions{URL: work}); err != nil {
		t.Fatalf("PlainClone(bare) error = %v", err)
	}
	t.Setenv("PATH", "")

	tests := []struct {
		// Name says what the remote names.
		Name string
		// URL is the project's remote.
		URL string
	}{{ // Test 0: A working copy named by its path, whose history is in .git.
		Name: "working copy", URL: work,
	}, { // Test 1: The same working copy as a file URL.
		Name: "file url", URL: "file://" + filepath.ToSlash(work),
	}, { // Test 2: A bare repository.
		Name: "bare", URL: bare,
	}}
	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			syncer, err := NewSyncer(t.TempDir())
			if err != nil {
				t.Fatalf("NewSyncer() error = %v", err)
			}
			p := &Project{ID: "proj_local_" + string(rune('a'+testNum)), Name: test.Name,
				RepoURL: test.URL, Branch: "main"}
			wt, err := syncer.Sync(p, "")
			if err != nil {
				t.Fatalf("Sync() error = %v", err)
			}
			defer wt.Cleanup()
			if wt.SHA != first {
				t.Errorf("synced commit = %s, want %s", wt.SHA, first)
			}
			body, err := os.ReadFile(filepath.Join(wt.Dir, "site.yml"))
			if err != nil || !strings.Contains(string(body), "hosts: all") {
				t.Errorf("site.yml in the checkout = %q (%v), want the committed playbook", body,
					err)
			}
		})
	}

	t.Run("fetch after a new commit", func(t *testing.T) {
		syncer, err := NewSyncer(t.TempDir())
		if err != nil {
			t.Fatalf("NewSyncer() error = %v", err)
		}
		p := &Project{ID: "proj_moves", Name: "moves", RepoURL: work, Branch: "main"}
		if got, err := syncer.Fetch(p, ""); err != nil || got != first {
			t.Fatalf("Fetch() = %s, %v, want %s", got, err, first)
		}
		second := commitLocal(t, work, map[string]string{"site.yml": "- hosts: web\n  tasks: []\n"})
		if got, err := syncer.Fetch(p, ""); err != nil || got != second {
			t.Errorf("Fetch() after a commit = %s, %v, want %s", got, err, second)
		}
	})
}

// TestALocalFolderThatHoldsNoHistoryIsRefused covers the folders the in-process transport cannot
// serve. A folder whose .git is a file, as in a linked worktree or a submodule, keeps its history
// elsewhere, so it is refused with an error that says so and what to point the project at. A
// folder that is no repository at all is not found.
func TestALocalFolderThatHoldsNoHistoryIsRefused(t *testing.T) {
	t.Parallel()
	linked := t.TempDir()
	if err := os.WriteFile(filepath.Join(linked, ".git"),
		[]byte("gitdir: /srv/git/infra/.git/worktrees/linked\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(.git) error = %v", err)
	}
	plain := t.TempDir()
	if err := os.WriteFile(filepath.Join(plain, "site.yml"), []byte("- hosts: all\n"),
		0o600); err != nil {
		t.Fatalf("WriteFile(site.yml) error = %v", err)
	}

	tests := []struct {
		// Dir is the folder the project's remote names.
		Dir string
		// Want is the error the sync fails with.
		Want error
		// WantText is what the error must say.
		WantText string
	}{{ // Test 0: A linked worktree.
		Dir: linked, Want: ErrLinkedCheckout, WantText: "linked worktree",
	}, { // Test 1: A folder that is no repository.
		Dir: plain, Want: transport.ErrRepositoryNotFound, WantText: "not found",
	}}
	for testNum, test := range tests {
		t.Run(test.WantText, func(t *testing.T) {
			t.Parallel()
			syncer, err := NewSyncer(t.TempDir())
			if err != nil {
				t.Fatalf("NewSyncer() error = %v", err)
			}
			p := &Project{ID: "proj_refused_" + string(rune('a'+testNum)), Name: "refused",
				RepoURL: test.Dir, Branch: "main"}
			_, err = syncer.Fetch(p, "")
			if !errors.Is(err, test.Want) || !strings.Contains(err.Error(), test.WantText) {
				t.Errorf("Fetch() error = %v, want %v saying %q", err, test.Want, test.WantText)
			}
		})
	}
}
