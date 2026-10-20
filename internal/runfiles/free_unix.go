//go:build linux || darwin

package runfiles

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// FreeBytes returns the bytes an unprivileged process may still write to the filesystem path is on.
// The blocks kept back for root are left out, since this process is not meant to fill them.
func FreeBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("%w: statfs %s: %w", ErrFreeSpace, path, err)
	}
	// The block size is signed on Linux and narrower on darwin, so it is widened before multiplying.
	return st.Bavail * uint64(st.Bsize), nil //nolint:gosec // A block size is never negative.
}
