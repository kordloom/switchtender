package project

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// initLinkedRepo builds a local git repository holding files and symbolic links, committed on main,
// and returns its path. links maps each link's path to the target it stores.
func initLinkedRepo(t *testing.T, files, links map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.Main},
	})
	if err != nil {
		t.Fatalf("PlainInitWithOptions() error = %v", err)
	}
	for rel, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", rel, err)
		}
	}
	for rel, target := range links {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", rel, err)
		}
		if err := os.Symlink(target, full); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree() error = %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob() error = %v", err)
	}
	if _, err := wt.Commit("first", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	return dir
}

// syncedProject syncs a project over repo into a fresh cache and returns the syncer, the project,
// and the commit the sync checked out.
func syncedProject(t *testing.T, repo string) (*Syncer, *Project, string) {
	t.Helper()
	s, err := NewSyncer(t.TempDir())
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	p := &Project{ID: "proj_read", Name: "infra", RepoURL: repo, Branch: "main"}
	return s, p, resync(t, s, p)
}

// resync syncs p again and returns the commit it checked out.
func resync(t *testing.T, s *Syncer, p *Project) string {
	t.Helper()
	wt, err := s.Sync(p, "")
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	wt.Cleanup()
	return wt.SHA
}

// readAt returns the content of name in the project at commit, or the error reading it.
func readAt(s *Syncer, projectID, commit, name string) (string, string, error) {
	var body []byte
	var sha string
	err := s.ReadCommit(projectID, commit, func(fsys fs.FS, got string) error {
		sha = got
		var rerr error
		body, rerr = fs.ReadFile(fsys, name)
		return rerr
	})
	return string(body), sha, err
}

// TestReadCommitReadsTheCommitItIsAskedFor is the property the grade depends on.
//
// A held run is pinned to the commit current when it was held, and it executes that commit or
// nothing. Reading the checkout's working tree instead graded whatever the last sync of the
// project had fetched, so once another run synced a newer commit, an approver was shown the grade
// of code the held run would never execute.
func TestReadCommitReadsTheCommitItIsAskedFor(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t, map[string]string{"site.yml": "first\n"})
	s, p, first := syncedProject(t, repo)
	commitTestFile(t, repo, "site.yml", "second\n")
	second := resync(t, s, p)
	if first == second {
		t.Fatal("the second commit did not change HEAD, so this test would assert nothing")
	}

	body, sha, err := readAt(s, p.ID, first, "site.yml")
	if err != nil {
		t.Fatalf("ReadCommit(first) error = %v", err)
	}
	if body != "first\n" || sha != first {
		t.Errorf("ReadCommit(first) = %q at %s, want the first commit's content at %s", body, sha, first)
	}

	body, sha, err = readAt(s, p.ID, "", "site.yml")
	if err != nil {
		t.Fatalf("ReadCommit(current) error = %v", err)
	}
	if body != "second\n" || sha != second {
		t.Errorf("ReadCommit(current) = %q at %s, want the current commit's content at %s",
			body, sha, second)
	}
}

// TestReadCommitRefusesWhatItCannotRead covers the refusals: no syncer, a project never synced, a
// value that is not a commit hash, and a hash the checkout does not hold.
func TestReadCommitRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t, map[string]string{"site.yml": "x\n"})
	s, p, _ := syncedProject(t, repo)
	var none *Syncer
	tests := []struct {
		Syncer  *Syncer
		Project string
		Commit  string
		Want    error
	}{{ // Test 0: A nil syncer.
		Syncer: none, Project: p.ID, Want: ErrNoCheckout,
	}, { // Test 1: A project never synced.
		Syncer: s, Project: "proj_never", Want: ErrNoCheckout,
	}, { // Test 2: A branch name rather than a commit.
		Syncer: s, Project: p.ID, Commit: "main", Want: ErrNoCommit,
	}, { // Test 3: A well-formed hash the checkout does not hold.
		Syncer: s, Project: p.ID, Commit: fmt.Sprintf("%040d", 7), Want: ErrNoCommit,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			called := false
			err := test.Syncer.ReadCommit(test.Project, test.Commit, func(fs.FS, string) error {
				called = true
				return nil
			})
			if !errors.Is(err, test.Want) {
				t.Errorf("ReadCommit() error = %v, want %v", err, test.Want)
			}
			if called {
				t.Error("ReadCommit() called fn despite refusing")
			}
		})
	}
}

// TestACommitsLinksStayInsideIt covers symbolic links, which a repository controls completely.
//
// A link inside the commit is followed, since roles are commonly shared that way. A link that
// leaves the commit, by climbing or by naming an absolute path, is refused, or a repository could
// have a file on the machine read as though it were one of its own. A link that never resolves
// fails rather than looping.
func TestACommitsLinksStayInsideIt(t *testing.T) {
	t.Parallel()
	repo := initLinkedRepo(t, map[string]string{
		"shared/roles/common/tasks/main.yml": "shared\n",
	}, map[string]string{
		"roles/common": "../shared/roles/common",
		"climb":        "../../../../../../../../etc",
		"absolute":     "/etc",
		"loop":         "loop",
		"here":         ".",
	})
	s, p, _ := syncedProject(t, repo)
	tests := []struct {
		Name     string
		WantBody string
		Want     error
	}{{ // Test 0: A link inside the commit is followed.
		Name: "roles/common/tasks/main.yml", WantBody: "shared\n",
	}, { // Test 1: A link to the root is followed.
		Name: "here/shared/roles/common/tasks/main.yml", WantBody: "shared\n",
	}, { // Test 2: A link that climbs out is refused.
		Name: "climb/passwd", Want: ErrOutsideCheckout,
	}, { // Test 3: A link to an absolute path is refused.
		Name: "absolute/passwd", Want: ErrOutsideCheckout,
	}, { // Test 4: A link that never resolves fails rather than looping.
		Name: "loop", Want: errTooManyLinks,
	}, { // Test 5: A name that is not there.
		Name: "missing.yml", Want: fs.ErrNotExist,
	}, { // Test 6: A file used as a directory.
		Name: "shared/roles/common/tasks/main.yml/x", Want: fs.ErrNotExist,
	}, { // Test 7: A name fs.FS does not accept.
		Name: "../site.yml", Want: fs.ErrInvalid,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			body, _, err := readAt(s, p.ID, "", test.Name)
			if test.Want != nil {
				if !errors.Is(err, test.Want) {
					t.Errorf("read %q error = %v, want %v", test.Name, err, test.Want)
				}
				return
			}
			if err != nil || body != test.WantBody {
				t.Errorf("read %q = %q, %v, want %q", test.Name, body, err, test.WantBody)
			}
		})
	}
}

// TestACommitDescribesItsEntries covers the parts of fs.FS the scan leans on to tell a role
// directory from a file: Stat through a link, and a directory that opens but has no content.
func TestACommitDescribesItsEntries(t *testing.T) {
	t.Parallel()
	repo := initLinkedRepo(t, map[string]string{
		"shared/roles/common/tasks/main.yml": "twelve bytes",
	}, map[string]string{"roles/common": "../shared/roles/common"})
	s, p, _ := syncedProject(t, repo)
	err := s.ReadCommit(p.ID, "", func(fsys fs.FS, _ string) error {
		dir, err := fs.Stat(fsys, "roles/common")
		if err != nil || !dir.IsDir() {
			t.Errorf("Stat(roles/common) = %v, %v, want a directory through the link", dir, err)
		}
		file, err := fs.Stat(fsys, "roles/common/tasks/main.yml")
		if err != nil || file.IsDir() || file.Size() != 12 || !file.Mode().IsRegular() {
			t.Errorf("Stat(main.yml) = %v, %v, want a 12 byte regular file", file, err)
		}
		root, err := fs.Stat(fsys, ".")
		if err != nil || !root.IsDir() {
			t.Errorf("Stat(.) = %v, %v, want the root directory", root, err)
		}
		f, err := fsys.Open("shared")
		if err != nil {
			t.Fatalf("Open(shared) error = %v", err)
		}
		defer func() { _ = f.Close() }()
		if _, err := io.ReadAll(f); err == nil {
			t.Error("reading a directory returned content")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ReadCommit() error = %v", err)
	}
}

// TestInstalledDependenciesReadBesideTheCommit covers the one thing a commit does not hold. A
// sync installs a project's Galaxy roles and collections into the checkout, and a run uses them,
// so a role that lives only there has to be readable beside the committed files or every
// dependency-heavy project grades as though its roles were empty.
func TestInstalledDependenciesReadBesideTheCommit(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t, map[string]string{"site.yml": "x\n"})
	s, p, _ := syncedProject(t, repo)
	installed := filepath.Join(s.cacheDir, p.ID, galaxyDir, "roles", "acme.db", "tasks")
	if err := os.MkdirAll(installed, 0o750); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	main := filepath.Join(installed, "main.yml")
	if err := os.WriteFile(main, []byte("installed\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	body, _, err := readAt(s, p.ID, "", ".galaxy/roles/acme.db/tasks/main.yml")
	if err != nil || body != "installed\n" {
		t.Errorf("installed role = %q, %v, want its content", body, err)
	}
	if body, _, err := readAt(s, p.ID, "", "site.yml"); err != nil || body != "x\n" {
		t.Errorf("committed file = %q, %v, want it still read from the commit", body, err)
	}
}

// TestFetchUpdatesTheCheckoutWithoutARunCopy covers the gate's fetch. It must leave the checkout on
// the branch's newest commit, as a sync does, without the per-run copy a sync makes for a run to
// execute in, since nothing executes in it.
func TestFetchUpdatesTheCheckoutWithoutARunCopy(t *testing.T) {
	t.Parallel()
	repo := initTestRepo(t, map[string]string{"site.yml": "first\n"})
	s, p, first := syncedProject(t, repo)
	commitTestFile(t, repo, "site.yml", "second\n")

	sha, err := s.Fetch(p, "")
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if sha == first {
		t.Fatal("Fetch() left the checkout on the first commit")
	}
	body, head, err := readAt(s, p.ID, "", "site.yml")
	if err != nil || body != "second\n" || head != sha {
		t.Errorf("after Fetch() the checkout reads %q at %s, %v, want the second commit %s",
			body, head, err, sha)
	}
	copies, err := os.ReadDir(filepath.Join(s.cacheDir, runsSubdir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(copies) != 0 {
		t.Errorf("Fetch() left %d run copies behind", len(copies))
	}
	if _, err := s.Fetch(&Project{ID: p.ID, RepoURL: "file:///etc"}, ""); err == nil {
		t.Error("Fetch() accepted a remote the URL check refuses")
	}
}
