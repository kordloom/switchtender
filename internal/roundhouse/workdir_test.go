package roundhouse

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestToolWorkDirFollowsSymlinksOutOfTheCheckout covers containment that was spelling-based. The check
// joined the subdirectory to the checkout and compared the two paths as text, so a subdirectory whose
// name is a symlink out of the checkout passed: the string sits under the base, the directory does not.
// A project's own repository is enough to place one, and whoever can commit to the repository a
// template runs can then aim terraform at any directory the server can read, or mount it into the
// container, since the mount is built from this same path.
func TestToolWorkDirFollowsSymlinksOutOfTheCheckout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	base := filepath.Join(root, "checkout")
	outside := filepath.Join(root, "elsewhere")
	for _, dir := range []string{base, outside, filepath.Join(base, "infra")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	// A subdirectory of the checkout that is really a link out of it, which is what a committed
	// symlink looks like after a clone.
	if err := os.Symlink(outside, filepath.Join(base, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// And one pointing at the filesystem root, the version that reaches everything.
	if err := os.Symlink("/", filepath.Join(base, "root")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	tests := []struct {
		Name string
		Sub  string
		Want error
	}{ // Test 0: An ordinary subdirectory is allowed.
		{"plain subdirectory", "infra", nil},
		// Test 1: The base itself is allowed.
		{"base", ".", nil},
		// Test 2: A lexical escape is refused, as it always was.
		{"dot dot", "../elsewhere", ErrBadWorkDir},
		// Test 3: A symlink out of the checkout is refused. It was not.
		{"symlink out", "escape", ErrBadWorkDir},
		// Test 4: A symlink to the root of the filesystem is refused.
		{"symlink to root", "root", ErrBadWorkDir},
		// Test 5: A path through a symlink is refused too, not only the link itself.
		{"path through symlink", "escape/deeper", ErrBadWorkDir},
		// Test 6: A subdirectory that does not exist yet is allowed, since terraform is run against a
		// path the project may create and refusing it would break an ordinary layout.
		{"not yet created", "infra/stage", nil},
	}
	for i, tc := range tests {
		got, err := toolWorkDir(base, tc.Sub)
		if tc.Want != nil {
			if !errors.Is(err, tc.Want) {
				t.Errorf("test %d (%s): toolWorkDir(%q) = (%q, %v), want %v",
					i, tc.Name, tc.Sub, got, err, tc.Want)
			}
			continue
		}
		if err != nil {
			t.Errorf("test %d (%s): toolWorkDir(%q) error = %v, want it allowed",
				i, tc.Name, tc.Sub, err)
		}
	}

	// With no checkout the subdirectory is used as given, which is the command-line case where the
	// operator names the directory themselves.
	if got, err := toolWorkDir("", "/srv/infra"); err != nil || got != "/srv/infra" {
		t.Errorf("toolWorkDir(\"\", \"/srv/infra\") = (%q, %v), want the path as given", got, err)
	}
}
