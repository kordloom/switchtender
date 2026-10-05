//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package runfiles

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// privateDir reports why path cannot serve as a private runtime directory: it must be a real
// directory, owned by this account, with no access for group or others.
func privateDir(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("is not an absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("cannot be read: %w", err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("is not a directory")
	}
	if uid, ok := ownerOf(info); ok && uid != os.Geteuid() {
		return fmt.Errorf("is owned by uid %d, not %d", uid, os.Geteuid())
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("is mode %04o, open to other accounts", info.Mode().Perm())
	}
	return nil
}

// checkAncestors confirms nobody but this account and the superuser can rename the root out from
// under the process. It walks every directory above the root's real location and refuses one owned
// by another account, or writable by group or others without the sticky bit, since either lets
// someone swap the root for a directory of their own between the moment it is checked and the
// moment a secret is written into it. A sticky directory such as /tmp is fine: only an entry's
// owner may rename it there.
func checkAncestors(root string) error {
	return checkAncestorsAs(root, os.Geteuid())
}

// checkAncestorsAs is checkAncestors for the account euid.
func checkAncestorsAs(root string, euid int) error {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUnsafeRoot, root, err)
	}
	for dir := filepath.Dir(real); ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil {
			return fmt.Errorf("%w: %s: %w", ErrUnsafeRoot, dir, err)
		}
		uid, ok := ownerOf(info)
		if ok && uid != 0 && uid != euid {
			return fmt.Errorf("%w: %s, above the root, is owned by uid %d, who could replace the root",
				ErrUnsafeRoot, dir, uid)
		}
		perm := info.Mode()
		if perm.Perm()&0o022 != 0 && perm&fs.ModeSticky == 0 {
			return fmt.Errorf("%w: %s, above the root, is mode %04o: writable by other accounts "+
				"without the sticky bit, so any of them could replace the root", ErrUnsafeRoot, dir,
				perm.Perm())
		}
		if parent := filepath.Dir(dir); parent == dir {
			return nil
		}
	}
}
