//go:build freebsd

package runfiles

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// FreeBytes returns the bytes an unprivileged process may still write to the filesystem path is on.
// FreeBSD reports the available blocks signed, negative once root's reserve is in use, which leaves
// nothing for this process.
func FreeBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("%w: statfs %s: %w", ErrFreeSpace, path, err)
	}
	if st.Bavail < 0 {
		return 0, nil
	}
	return uint64(st.Bavail) * st.Bsize, nil
}
