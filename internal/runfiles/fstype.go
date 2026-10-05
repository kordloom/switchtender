package runfiles

// fsClass sorts a filesystem by whether run files may live on it.
type fsClass int

const (
	// classUnknown is a filesystem SwitchTender does not know, refused because nobody has shown its
	// locks hold.
	classUnknown fsClass = iota
	// classLocal is a known local filesystem whose locks the kernel keeps.
	classLocal
	// classNetwork is a filesystem shared with another machine, where a lock can be lost or faked and
	// a sweep could then delete a live run's credentials.
	classNetwork
)

// filesystem is what Prepare learned about the filesystem under a root.
type filesystem struct {
	// Name is the filesystem's type, such as ext4, tmpfs, apfs, or NTFS.
	Name string
	// Class says whether run files may live on it.
	Class fsClass
	// Memory reports that it is memory-backed, so its contents never reach a disk except through swap.
	Memory bool
}
