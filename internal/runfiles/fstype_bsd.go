//go:build darwin || freebsd

package runfiles

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// bsdLocal names the filesystem types known to be local, and which of them are memory-backed. The
// kernel's own local flag must agree before one is accepted.
var bsdLocal = map[string]bool{
	"apfs": false, "hfs": false, "ufs": false, "zfs": false, "tmpfs": true,
}

// bsdNetwork names the network filesystem types, refused even if the kernel were to call one local.
var bsdNetwork = map[string]bool{
	"nfs": true, "smbfs": true, "afpfs": true, "webdav": true, "ftp": true, "cifs": true,
}

// filesystemOf reads the type of the filesystem path is on.
func filesystemOf(path string) (filesystem, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return filesystem{}, fmt.Errorf("statfs %s: %w", path, err)
	}
	name := unix.ByteSliceToString(st.Fstypename[:])
	if bsdNetwork[name] {
		return filesystem{Name: name, Class: classNetwork}, nil
	}
	memory, known := bsdLocal[name]
	if !known || uint64(st.Flags)&unix.MNT_LOCAL == 0 {
		return filesystem{Name: name, Class: classUnknown}, nil
	}
	return filesystem{Name: name, Class: classLocal, Memory: memory}, nil
}
