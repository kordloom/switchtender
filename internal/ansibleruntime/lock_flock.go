//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package ansibleruntime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockRoot takes an exclusive flock on the root's lock file, waiting for any other install or
// remove under the same root to finish, and returns the function that releases it. The kernel
// drops the lock when its holder exits, so a killed install never leaves the root locked.
//
// The file is opened without following a symlink and must be a regular file, so a .lock planted as
// a link cannot make an install run as root create the file it names. A remove of everything
// unlinks the lock file while holding it, so once the lock is held the open file is checked to
// still be the one at the path, and the lock is taken again when it is not.
func lockRoot(root string) (func(), error) {
	path := filepath.Join(root, ".lock")
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return nil, fmt.Errorf("%w: open %s: %w", ErrUntrusted, path, err)
		}
		held, err := flockAndCheck(f, path)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		if held {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		_ = f.Close()
	}
}

// flockAndCheck takes an exclusive flock on f, which was opened from path, and reports whether f is
// still the regular file at path once the lock is held. It returns false, with the lock released,
// when the path now names another file or none.
func flockAndCheck(f *os.File, path string) (bool, error) {
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%w: %s is not a regular file", ErrUntrusted, path)
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EINTR) {
			return false, err
		}
	}
	now, err := os.Lstat(path)
	if err == nil && os.SameFile(info, now) {
		return true, nil
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, nil
}
