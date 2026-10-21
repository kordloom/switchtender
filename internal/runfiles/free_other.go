//go:build !(linux || darwin || freebsd || windows)

package runfiles

import "fmt"

// FreeBytes reports that free space cannot be read on a platform SwitchTender does not ship for.
func FreeBytes(path string) (uint64, error) {
	return 0, fmt.Errorf("%w: %s: not supported on this platform", ErrFreeSpace, path)
}
