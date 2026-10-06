//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package runfiles

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPrepareRefusesARootAnotherAccountOwns proves a root, or a directory above it, owned by
// someone other than this account and the superuser is refused. The root case points at a directory
// the superuser owns, which this account cannot have made. The ancestor case checks a real tree
// against an account that is not its owner. It builds only where checkAncestorsAs does, the
// platforms whose owner and mode prove a directory private.
func TestPrepareRefusesARootAnotherAccountOwns(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("needs a POSIX owner that is not this account")
	}
	if _, err := prepare("/", checks{classify: localFS, lock: lockFile}); !errors.Is(err,
		ErrUnsafeRoot) || !strings.Contains(err.Error(), "owned by uid") {
		t.Errorf("prepare(/) error = %v, want a root the superuser owns refused", err)
	}
	tree := filepath.Join(t.TempDir(), "mine", "runfiles")
	if err := os.MkdirAll(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkAncestorsAs(tree, os.Geteuid()); err != nil {
		t.Fatalf("checkAncestorsAs() for the owner error = %v", err)
	}
	if err := checkAncestorsAs(tree, os.Geteuid()+1); !errors.Is(err, ErrUnsafeRoot) {
		t.Errorf("checkAncestorsAs() for another account error = %v, want the tree refused", err)
	}
}
