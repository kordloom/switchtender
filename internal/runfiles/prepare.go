package runfiles

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
)

// probePrefix starts the name of the lock probe file, which a sweep never mistakes for a run.
const probePrefix = ".probe-"

// Report is what Prepare proved about a root.
type Report struct {
	// Root is the root's path.
	Root string
	// Filesystem is the type of the filesystem the root sits on.
	Filesystem string
	// MemoryBacked reports that the filesystem keeps its contents in memory, so a run's files reach a
	// disk only if the host swaps.
	MemoryBacked bool
}

// checks holds what Prepare relies on from the operating system, so a test can stand in for a
// filesystem SwitchTender does not know or one whose locks do not hold.
type checks struct {
	// classify reads the type of the filesystem a path is on.
	classify func(string) (filesystem, error)
	// lock is the lock call run directories use.
	lock func(*os.File, bool) (bool, error)
}

// systemChecks are the operating system's own answers.
var systemChecks = checks{classify: filesystemOf, lock: lockFile}

// Prepare creates root when it is missing and proves it safe to stage secrets in before the process
// serves. The root must be an absolute path outside /dev/shm, a real directory owned by this
// account and closed to everyone else, below no directory another account could rename it out of,
// and on a known local filesystem. A network filesystem and an unknown one are refused, the unknown
// one even though its locks might work, because a lock that misjudges a live run as dead would let
// a sweep delete credentials out from under a running job. Last, a probe takes a lock exactly the
// way a run directory's lock is taken and proves a second handle is kept out of it until it is
// released.
func Prepare(root string) (Report, error) {
	return prepare(root, systemChecks)
}

// prepare is Prepare with the operating system's answers supplied by the caller.
func prepare(root string, c checks) (Report, error) {
	if !filepath.IsAbs(root) {
		return Report{}, fmt.Errorf("%w: %s is not an absolute path", ErrUnsafeRoot, root)
	}
	if err := refuseDevShm(root); err != nil {
		return Report{}, err
	}
	if err := prepareRoot(root); err != nil {
		return Report{}, err
	}
	// The real location is checked again once it exists, so a link that leads into /dev/shm is caught
	// as surely as the path written out.
	if real, err := filepath.EvalSymlinks(root); err == nil {
		if err := refuseDevShm(real); err != nil {
			return Report{}, err
		}
	}
	if err := checkAncestors(root); err != nil {
		return Report{}, err
	}
	fs, err := c.classify(root)
	if err != nil {
		return Report{}, fmt.Errorf("%w: %s: %w", ErrFilesystem, root, err)
	}
	switch fs.Class {
	case classNetwork:
		return Report{}, fmt.Errorf("%w: %s is on %s, a network filesystem, where a lock can be lost "+
			"or held by another machine, so a sweep could delete a live run's credentials; point "+
			"--runfiles-dir at a local directory, a tmpfs for preference", ErrFilesystem, root, fs.Name)
	case classUnknown:
		return Report{}, fmt.Errorf("%w: %s is on %s, a filesystem SwitchTender does not know to be "+
			"local, so it cannot trust a lock there to say whether a run is alive; point "+
			"--runfiles-dir at a directory on tmpfs, ext4, xfs, btrfs, zfs, APFS, or NTFS",
			ErrFilesystem, root, fs.Name)
	}
	if err := probeLock(root, c.lock); err != nil {
		return Report{}, err
	}
	return Report{Root: root, Filesystem: fs.Name, MemoryBacked: fs.Memory}, nil
}

// refuseDevShm refuses a root in /dev/shm, which every process on the host shares and which a
// container sizes for shared memory rather than for anybody's secrets.
func refuseDevShm(path string) error {
	if underDevShm(path) {
		return fmt.Errorf("%w: %s is in /dev/shm, which every process on the host shares; use a "+
			"systemd RuntimeDirectory, a private XDG_RUNTIME_DIR, or a dedicated tmpfs with "+
			"--runfiles-dir", ErrUnsafeRoot, path)
	}
	return nil
}

// probeLock takes a lock in root with the same open mode and lock call a run directory uses, and
// proves the filesystem keeps a second handle out until the first lets go and lets it in after.
func probeLock(root string, lock func(*os.File, bool) (bool, error)) error {
	path := filepath.Join(root, probePrefix+strconv.Itoa(os.Getpid())+"-"+
		strconv.FormatUint(rand.Uint64(), 36))
	first, err := createLockFile(path)
	if err != nil {
		return fmt.Errorf("%w: create %s: %w", ErrLockProbe, path, err)
	}
	var second *os.File
	defer func() {
		_ = first.Close()
		if second != nil {
			_ = second.Close()
		}
		_ = os.Remove(path)
	}()
	if held, err := lock(first, true); err != nil || !held {
		return fmt.Errorf("%w: the first handle could not take the lock on %s: %w", ErrLockProbe,
			root, errors.Join(err, errNotHeld(held)))
	}
	second, err = openLockFile(path)
	if err != nil {
		return fmt.Errorf("%w: open a second handle on %s: %w", ErrLockProbe, root, err)
	}
	got, err := lock(second, false)
	if err != nil {
		return fmt.Errorf("%w: a second handle on %s: %w", ErrLockProbe, root, err)
	}
	if got {
		return fmt.Errorf("%w: a second handle on %s took a lock the first still held, so a lock "+
			"there cannot tell a live run from a dead one; point --runfiles-dir at a local "+
			"filesystem", ErrLockProbe, root)
	}
	_ = unlockFile(first)
	if got, err = lock(second, false); err != nil || !got {
		return fmt.Errorf("%w: a lock released on %s could not be taken again, so no run directory "+
			"there could ever be swept: %w", ErrLockProbe, root, errors.Join(err, errNotHeld(got)))
	}
	_ = unlockFile(second)
	return nil
}

// errNotHeld returns an error saying a lock was not taken, or nil when it was.
func errNotHeld(held bool) error {
	if held {
		return nil
	}
	return errors.New("the lock was not taken")
}
