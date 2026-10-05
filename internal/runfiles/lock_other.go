//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows)

package runfiles

import (
	"io/fs"
	"os"
)

// lockFile reports the lock as taken for its creator and as held by someone else for a sweep. A
// platform with no file lock cannot tell a live run from a dead one, so a sweep there removes only
// lockless directories past their grace and never a directory a run may still be using.
func lockFile(_ *os.File, block bool) (bool, error) { return block, nil }

// unlockFile does nothing on a platform with no file lock.
func unlockFile(*os.File) error { return nil }

// checkOwner accepts the directory on a platform with no owner to compare.
func checkOwner(fs.FileInfo) error { return nil }

// ownerOf reports no owner on a platform with no owner to compare.
func ownerOf(fs.FileInfo) (int, bool) { return 0, false }
