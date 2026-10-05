//go:build unix

package roundhouse

import (
	"io/fs"
	"os"
	"syscall"
)

// ownedBySelf reports whether the file described by info belongs to this process's account.
func ownedBySelf(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}
