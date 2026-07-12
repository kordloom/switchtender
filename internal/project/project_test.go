package project_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcadolph/yardmaster/internal/project"
	"github.com/dcadolph/yardmaster/internal/projecttest"
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

	dir, sha1, _, err := s.Sync(p, "")
	if err != nil {
		t.Fatalf("Sync() clone error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "site.yml")); err != nil {
		t.Fatalf("checkout missing site.yml: %v", err)
	}
	if len(sha1) != 40 {
		t.Fatalf("sha = %q, want a full commit hash", sha1)
	}

	// A new commit upstream shows up on the next sync with a new hash.
	if err := os.WriteFile(filepath.Join(repo, "site.yml"), []byte("---\n# v2\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	runGit(t, repo, "commit", "-am", "second")

	dir2, sha2, _, err := s.Sync(p, "")
	if err != nil {
		t.Fatalf("Sync() update error = %v", err)
	}
	if dir2 != dir {
		t.Errorf("checkout moved from %s to %s", dir, dir2)
	}
	if sha2 == sha1 {
		t.Error("sha did not advance after a new commit")
	}
	body, err := os.ReadFile(filepath.Join(dir, "site.yml"))
	if err != nil {
		t.Fatalf("read synced file: %v", err)
	}
	if !strings.Contains(string(body), "v2") {
		t.Errorf("synced content = %q, want the second commit's content", body)
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
		{In: "https://github.com/org/repo.git", Want: nil},                            // Test 0: HTTPS.
		{In: "ssh://git@github.com/org/repo.git", Want: nil},                          // Test 1: SSH scheme.
		{In: "git@github.com:org/repo.git", Want: nil},                                // Test 2: scp-like SSH shorthand.
		{In: "/srv/git/repo.git", Want: nil},                                          // Test 3: Local path.
		{In: "file:///srv/git/repo.git", Want: nil},                                   // Test 4: File scheme.
		{In: "https://192.168.10.5/git/repo.git", Want: nil},                          // Test 5: Private host is a valid self-hosted git.
		{In: "", Want: project.ErrBadRepoURL},                                         // Test 6: Empty.
		{In: "http://github.com/org/repo.git", Want: project.ErrBadRepoURL},           // Test 7: Cleartext http refused.
		{In: "git://github.com/org/repo.git", Want: project.ErrBadRepoURL},            // Test 8: git protocol refused.
		{In: "https://169.254.169.254/latest/meta-data", Want: project.ErrBadRepoURL}, // Test 9: Cloud metadata IP.
		{In: "https://metadata.google.internal/x", Want: project.ErrBadRepoURL},       // Test 10: Metadata hostname.
		{In: "https://localhost/x", Want: project.ErrBadRepoURL},                      // Test 11: Loopback name.
		{In: "https://127.0.0.1/x", Want: project.ErrBadRepoURL},                      // Test 12: Loopback address.
		{In: "git@169.254.169.254:x", Want: project.ErrBadRepoURL},                    // Test 13: scp-like to metadata.
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
