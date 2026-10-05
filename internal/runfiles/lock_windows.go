//go:build windows

package runfiles

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive lock on the first byte of f, waiting for it when block is set and
// otherwise reporting false when another handle holds it. Windows releases the lock when the holding
// process exits.
func lockFile(f *os.File, block bool) (bool, error) {
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if !block {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, new(windows.Overlapped))
	switch {
	case err == nil:
		return true, nil
	case !block && (errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_IO_PENDING)):
		return false, nil
	default:
		return false, err
	}
}

// unlockFile releases the lock on f.
func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}

// checkOwner accepts the directory. The default root sits in the account's own temporary directory,
// whose access list already excludes other accounts.
func checkOwner(fs.FileInfo) error { return nil }

// ownerOf reports no owner, since Windows describes ownership with an access list rather than a
// uid.
func ownerOf(fs.FileInfo) (int, bool) { return 0, false }
