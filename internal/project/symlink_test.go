package project

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestWithinRepoResolvesSymlinks pins that a symlink committed to a repository cannot name a file
// outside the checkout.
//
// Git stores a symlink's target verbatim, so a repository can commit "esc -> /" and then name a
// playbook of "esc/etc/shadow". Every string test says that path is inside the checkout; the file
// that opens is not. The result goes straight to ansible-playbook and to ansible-inventory, so the
// repository would be choosing what the control node reads.
func TestWithinRepoResolvesSymlinks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("stolen"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	// A repository that commits a symlink pointing out of itself.
	if err := os.Symlink(outside, filepath.Join(root, "esc")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// And one pointing at the filesystem root.
	if err := os.Symlink(string(filepath.Separator), filepath.Join(root, "slash")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	for _, rel := range []string{"esc/secret.txt", "esc", "slash/etc/hosts", "slash"} {
		if got, err := WithinRepo(root, rel); !errors.Is(err, ErrEscapesRepo) {
			t.Errorf("WithinRepo(%q) = (%q, %v), want ErrEscapesRepo: a committed symlink chose "+
				"what the control node reads", rel, got, err)
		}
	}

	// Ordinary paths still work, including one that does not exist yet, because a sync writes files
	// after a template names them.
	if err := os.MkdirAll(filepath.Join(root, "plays"), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "plays", "site.yml"), []byte("---"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	for _, rel := range []string{"plays/site.yml", "plays", "not/written/yet.yml"} {
		if _, err := WithinRepo(root, rel); err != nil {
			t.Errorf("WithinRepo(%q) refused an ordinary path: %v", rel, err)
		}
	}
	// Traversal and absolute paths are still refused.
	for _, rel := range []string{"../outside.yml", "/etc/passwd", "plays/../../x"} {
		if _, err := WithinRepo(root, rel); !errors.Is(err, ErrEscapesRepo) {
			t.Errorf("WithinRepo(%q) was allowed", rel)
		}
	}
}

// TestGitDirectoryIsRefusedEveryWayIn pins that the git directory cannot be read through a symlink,
// a case variant, or a nested path.
//
// The refusal checked only the first path segment, only in the case it was written, and only before
// symlinks were resolved. A committed "gitdir -> .git" served the config file, ".GIT/config" served
// it on any case-insensitive filesystem, and a submodule's "sub/.git/config" was never a first
// segment. That file holds credential helper settings and remote URLs.
func TestGitDirectoryIsRefusedEveryWayIn(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{
		".git/config",
		".GIT/config",
		".Git/config",
		"sub/.git/config",
		"a/b/.git/config",
		".git",
	} {
		if !isGitPath(rel) {
			t.Errorf("isGitPath(%q) = false; the git directory is readable that way", rel)
		}
	}
	for _, rel := range []string{
		"playbooks/site.yml",
		"gitignore",
		".gitignore",
		"roles/git/tasks/main.yml",
		"my.git.yml",
	} {
		if isGitPath(rel) {
			t.Errorf("isGitPath(%q) = true; an ordinary file was refused", rel)
		}
	}
}

// TestRepoURLRefusesOptionsAndLoopbackShorthands pins the two classes the validator inferred past.
//
// A value beginning with a dash was classified as a local file path and accepted, so
// "--upload-pack=/bin/sh" passed a function whose job is deciding what is safe to clone. It is inert
// while go-git is the driver and becomes execution the day anything shells out to git. Separately,
// net.ParseIP returns nil for "127.1" and "2130706433", so the loopback tests never ran on the two
// shorthand spellings a resolver honors.
func TestRepoURLRefusesOptionsAndLoopbackShorthands(t *testing.T) {
	t.Parallel()
	refused := []string{
		"--upload-pack=/bin/sh",
		"-u",
		"--config=core.gitProxy=/bin/sh",
		"https://127.1/x",
		"https://2130706433/x",
		"https://127.0.0.1/x",
		"https://169.254.169.254/latest/meta-data/",
	}
	for _, raw := range refused {
		if err := ValidateRepoURL(raw); err == nil {
			t.Errorf("ValidateRepoURL(%q) accepted it", raw)
		}
	}
	allowed := []string{
		"https://github.com/org/repo.git",
		"git@github.com:org/repo.git",
		"ssh://git@example.com/org/repo.git",
		"https://8.8.8.8/org/repo.git",
	}
	for _, raw := range allowed {
		if err := ValidateRepoURL(raw); err != nil {
			t.Errorf("ValidateRepoURL(%q) refused an ordinary repository: %v", raw, err)
		}
	}
}
