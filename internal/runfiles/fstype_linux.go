//go:build linux

package runfiles

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// linuxFilesystems maps the statfs magic numbers of the filesystems SwitchTender knows to what they
// are. Anything missing is unknown and refused.
//
// Overlay is accepted because a container's own filesystem is one. From inside the container
// nothing shows what lies under it, so for overlay the startup lock probe is what shows its locks
// hold. FUSE is unknown, since the program behind it decides what a lock means.
var linuxFilesystems = map[uint32]filesystem{
	0xEF53:     {Name: "ext4", Class: classLocal},
	0x58465342: {Name: "xfs", Class: classLocal},
	0x9123683E: {Name: "btrfs", Class: classLocal},
	0x2FC12FC1: {Name: "zfs", Class: classLocal},
	0x01021994: {Name: "tmpfs", Class: classLocal, Memory: true},
	0x858458F6: {Name: "ramfs", Class: classLocal, Memory: true},
	0x794C7630: {Name: "overlay", Class: classLocal},
	0xF2F52010: {Name: "f2fs", Class: classLocal},
	0xCA451A4E: {Name: "bcachefs", Class: classLocal},
	0x3153464A: {Name: "jfs", Class: classLocal},
	0x52654973: {Name: "reiserfs", Class: classLocal},
	0x3434:     {Name: "nilfs2", Class: classLocal},
	0x6969:     {Name: "nfs", Class: classNetwork},
	0x517B:     {Name: "smb", Class: classNetwork},
	0xFF534D42: {Name: "cifs", Class: classNetwork},
	0xFE534D42: {Name: "smb2", Class: classNetwork},
	0x00C36400: {Name: "ceph", Class: classNetwork},
	0x5346414F: {Name: "afs", Class: classNetwork},
	0x6B414653: {Name: "kafs", Class: classNetwork},
	0x73757245: {Name: "coda", Class: classNetwork},
	0x01021997: {Name: "9p", Class: classNetwork},
	0x6A656A63: {Name: "virtiofs", Class: classNetwork},
	0x786F4256: {Name: "vboxsf", Class: classNetwork},
	0x01161970: {Name: "gfs2", Class: classNetwork},
	0x7461636F: {Name: "ocfs2", Class: classNetwork},
	0x0BD00BD0: {Name: "lustre", Class: classNetwork},
	0x47504653: {Name: "gpfs", Class: classNetwork},
	0x65735546: {Name: "fuse", Class: classUnknown},
}

// filesystemOf reads the type of the filesystem path is on.
func filesystemOf(path string) (filesystem, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return filesystem{}, fmt.Errorf("statfs %s: %w", path, err)
	}
	// The field is signed on some architectures, so the magic number is compared as its 32 bits.
	magic := uint32(st.Type)
	if fs, ok := linuxFilesystems[magic]; ok {
		return fs, nil
	}
	return filesystem{Name: fmt.Sprintf("0x%X", magic), Class: classUnknown}, nil
}
