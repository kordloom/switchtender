//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package runfiles

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// lockFile takes an exclusive flock on f, waiting for it when block is set and otherwise reporting
// false when another open file holds it. A flock belongs to the open file, so a second open of the
// same file conflicts even inside one process, and the kernel drops it when its holder exits.
func lockFile(f *os.File, block bool) (bool, error) {
	how := syscall.LOCK_EX
	if !block {
		how |= syscall.LOCK_NB
	}
	for {
		err := syscall.Flock(int(f.Fd()), how)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case !block && errors.Is(err, syscall.EWOULDBLOCK):
			return false, nil
		default:
			return false, err
		}
	}
}

// unlockFile releases the flock on f.
func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// checkOwner confirms the directory described by info belongs to this process's account.
func checkOwner(info fs.FileInfo) error {
	uid, ok := ownerOf(info)
	if !ok {
		return nil
	}
	if uid != os.Geteuid() {
		return fmt.Errorf("owned by uid %d, not %d", uid, os.Geteuid())
	}
	return nil
}

// ownerOf returns the account that owns the file described by info, and false when the platform
// does not say.
func ownerOf(info fs.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
