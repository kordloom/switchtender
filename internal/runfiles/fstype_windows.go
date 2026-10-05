//go:build windows

package runfiles

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

// filesystemOf reads the type of the filesystem path is on. A network drive or share is refused by
// its drive type whatever it calls itself, and of the rest only NTFS and ReFS are local filesystems
// that keep the access list that makes a run directory private: FAT and exFAT have none.
func filesystemOf(path string) (filesystem, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return filesystem{}, fmt.Errorf("volume of %s: %w", path, err)
	}
	vol := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumePathName(p, &vol[0], uint32(len(vol))); err != nil {
		return filesystem{}, fmt.Errorf("volume of %s: %w", path, err)
	}
	if windows.GetDriveType(&vol[0]) == windows.DRIVE_REMOTE {
		return filesystem{Name: "remote", Class: classNetwork}, nil
	}
	name := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumeInformation(&vol[0], nil, 0, nil, nil, nil, &name[0],
		uint32(len(name))); err != nil {
		return filesystem{}, fmt.Errorf("volume information for %s: %w", path, err)
	}
	fsName := windows.UTF16ToString(name)
	memory := windows.GetDriveType(&vol[0]) == windows.DRIVE_RAMDISK
	switch strings.ToUpper(fsName) {
	case "NTFS", "REFS":
		return filesystem{Name: fsName, Class: classLocal, Memory: memory}, nil
	default:
		return filesystem{Name: fsName, Class: classUnknown, Memory: memory}, nil
	}
}
