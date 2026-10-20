//go:build windows

package runfiles

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// FreeBytes returns the bytes this process may still write to the volume path is on, which a quota
// can hold below the volume's own free space.
func FreeBytes(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: %w", ErrFreeSpace, path, err)
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, fmt.Errorf("%w: %s: %w", ErrFreeSpace, path, err)
	}
	return avail, nil
}
