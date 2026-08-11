package project_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/projecttest"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	projecttest.Contract(t, func() project.Store { return project.NewMemStore() })
}

// initRepo builds a local git repository with one committed file and returns its path.
func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "site.yml"), []byte("---\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "first")
	return dir
}

// runGit runs a git command in dir and fails the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestSyncerCloneAndUpdate(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	s, err := project.NewSyncer(t.TempDir())
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	p := &project.Project{ID: "proj_x", RepoURL: repo, Branch: "main"}

	wt1, err := s.Sync(p, "")
	if err != nil {
		t.Fatalf("Sync() clone error = %v", err)
	}
	defer wt1.Cleanup()
	if _, err := os.Stat(filepath.Join(wt1.Dir, "site.yml")); err != nil {
		t.Fatalf("checkout missing site.yml: %v", err)
	}
	if len(wt1.SHA) != 40 {
		t.Fatalf("sha = %q, want a full commit hash", wt1.SHA)
	}
	// The worktree is isolated, so .git is not copied into it.
	if _, err := os.Stat(filepath.Join(wt1.Dir, ".git")); !os.IsNotExist(err) {
		t.Errorf(".git was copied into the run checkout, err = %v", err)
	}

	// A new commit upstream shows up on the next sync with a new hash.
	if err := os.WriteFile(filepath.Join(repo, "site.yml"), []byte("---\n# v2\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	runGit(t, repo, "commit", "-am", "second")

	wt2, err := s.Sync(p, "")
	if err != nil {
		t.Fatalf("Sync() update error = %v", err)
	}
	defer wt2.Cleanup()
	// Each run gets its own checkout, so the second sync does not reuse the first's directory.
	if wt2.Dir == wt1.Dir {
		t.Errorf("the second run shares the first's checkout %s, so a concurrent run could change "+
			"the files under it", wt1.Dir)
	}
	if wt2.SHA == wt1.SHA {
		t.Error("sha did not advance after a new commit")
	}
	// The first worktree still holds the first commit's content: the update did not reach into it.
	first, err := os.ReadFile(filepath.Join(wt1.Dir, "site.yml"))
	if err != nil {
		t.Fatalf("read first checkout: %v", err)
	}
	if strings.Contains(string(first), "v2") {
		t.Errorf("the second sync changed the first run's checkout, which is the race this prevents:\n%s",
			first)
	}
	// The second worktree holds the second commit's content.
	second, err := os.ReadFile(filepath.Join(wt2.Dir, "site.yml"))
	if err != nil {
		t.Fatalf("read second checkout: %v", err)
	}
	if !strings.Contains(string(second), "v2") {
		t.Errorf("second checkout content = %q, want the second commit's content", second)
	}
}

func TestWithinRepo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Rel  string
		Want error
	}{
		{Rel: "site.yml", Want: nil},                                  // Test 0: Plain file.
		{Rel: "plays/deploy.yml", Want: nil},                          // Test 1: Nested file.
		{Rel: "../outside.yml", Want: project.ErrEscapesRepo},         // Test 2: Traversal.
		{Rel: "plays/../../etc/passwd", Want: project.ErrEscapesRepo}, // Test 3: Deep traversal.
		{Rel: "/etc/passwd", Want: project.ErrEscapesRepo},            // Test 4: Absolute path.
	}
	for i, test := range tests {
		_, err := project.WithinRepo("/srv/cache/proj_1", test.Rel)
		if !errors.Is(err, test.Want) {
			t.Errorf("test %d: WithinRepo(%q) error = %v, want %v", i, test.Rel, err, test.Want)
		}
	}
}

func TestValidateRepoURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   string
		Want error
	}{
		{In: "https://github.com/org/repo.git", Want: nil},                                // Test 0: HTTPS.
		{In: "ssh://git@github.com/org/repo.git", Want: nil},                              // Test 1: SSH scheme.
		{In: "git@github.com:org/repo.git", Want: nil},                                    // Test 2: scp-like SSH shorthand.
		{In: "/srv/git/repo.git", Want: nil},                                              // Test 3: Local path.
		{In: "file:///srv/git/repo.git", Want: nil},                                       // Test 4: File scheme.
		{In: "https://192.168.10.5/git/repo.git", Want: nil},                              // Test 5: Private host is a valid self-hosted git.
		{In: "", Want: project.ErrBadRepoURL},                                             // Test 6: Empty.
		{In: "http://github.com/org/repo.git", Want: project.ErrBadRepoURL},               // Test 7: Cleartext http refused.
		{In: "git://github.com/org/repo.git", Want: project.ErrBadRepoURL},                // Test 8: git protocol refused.
		{In: "https://169.254.169.254/latest/meta-data", Want: project.ErrBadRepoURL},     // Test 9: Cloud metadata IP.
		{In: "https://metadata.google.internal/x", Want: project.ErrBadRepoURL},           // Test 10: Metadata hostname.
		{In: "https://localhost/x", Want: project.ErrBadRepoURL},                          // Test 11: Loopback name.
		{In: "https://127.0.0.1/x", Want: project.ErrBadRepoURL},                          // Test 12: Loopback address.
		{In: "git@169.254.169.254:x", Want: project.ErrBadRepoURL},                        // Test 13: scp-like to metadata.
		{In: "https://oauth2:tok3n@github.com/org/repo.git", Want: project.ErrBadRepoURL}, // Test 14: Password in url.
		{In: "https://ghp_token@github.com/org/repo.git", Want: project.ErrBadRepoURL},    // Test 15: Token as https username.
		{In: "ssh://git:pass@github.com/org/repo.git", Want: project.ErrBadRepoURL},       // Test 16: Password on ssh.
	}
	for i, test := range tests {
		if err := project.ValidateRepoURL(test.In); !errors.Is(err, test.Want) {
			t.Errorf("test %d: ValidateRepoURL(%q) error = %v, want %v", i, test.In, err, test.Want)
		}
	}
}

// TestSyncIsolatesConcurrentRuns proves the provenance guarantee under concurrency: a run's checkout
// holds the commit stamped on it, and a second sync advancing the project does not change the files
// the first run is still reading.
//
// The checkout used to be shared per project and hard reset in place on every sync. A run recorded
// the commit it synced to and then executed out of that shared directory with no lock held, so a
// second run of the same project reset the tree mid-execution and the first run ran a mix of two
// commits while its audit record named only one. That is the audit trail asserting something the
// files on disk did not support, which is the one failure this product cannot have.
func TestSyncIsolatesConcurrentRuns(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	s, err := project.NewSyncer(t.TempDir())
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	p := &project.Project{ID: "proj_race", RepoURL: repo, Branch: "main"}

	// Run one syncs the first commit and keeps its checkout open, as a live run would.
	wt1, err := s.Sync(p, "")
	if err != nil {
		t.Fatalf("first Sync() error = %v", err)
	}
	defer wt1.Cleanup()
	firstContent, err := os.ReadFile(filepath.Join(wt1.Dir, "site.yml"))
	if err != nil {
		t.Fatalf("read first checkout: %v", err)
	}

	// A new commit lands and run two syncs it while run one is still holding its checkout.
	if err := os.WriteFile(filepath.Join(repo, "site.yml"), []byte("---\n# rewritten\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	runGit(t, repo, "commit", "-am", "rewrite")
	wt2, err := s.Sync(p, "")
	if err != nil {
		t.Fatalf("second Sync() error = %v", err)
	}
	defer wt2.Cleanup()

	// Run one's checkout is untouched: same directory, same bytes, and its recorded commit still
	// describes what is on disk.
	afterContent, err := os.ReadFile(filepath.Join(wt1.Dir, "site.yml"))
	if err != nil {
		t.Fatalf("re-read first checkout: %v", err)
	}
	if string(afterContent) != string(firstContent) {
		t.Errorf("the first run's files changed after a second sync:\nbefore %q\nafter  %q",
			firstContent, afterContent)
	}
	if strings.Contains(string(afterContent), "rewritten") {
		t.Error("the second sync's content reached the first run's checkout, so its recorded commit " +
			"no longer matches the files it executed")
	}
	if wt1.SHA == wt2.SHA {
		t.Error("both runs recorded the same commit, so the isolation cannot be observed")
	}
}
