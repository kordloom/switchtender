// Package runfiles keeps the secret material a run writes to disk in one private directory per run:
// credential files, keys, vars files, scripts, and the state cloud command line tools keep about
// the credentials they were handed. The directory is created mode 0700 and each file in it 0600, so
// only the account the executor runs as can read them, and the whole directory is removed when the
// run ends, on every path a run can end by.
//
// A process that dies mid-run cannot remove anything, so every directory carries a lock its owner
// holds for as long as the run lasts, and the operating system releases that lock when the owner
// dies, however it dies. The lock is the liveness signal and the only one. Each directory also
// carries a heartbeat counter its owner increments on a timer, and a sweep deletes a directory only
// after seeing its lock free and its counter unchanged twice, at least ObservationGap apart on the
// sweeper's own monotonic clock. The counter can hold a deletion back, never cause one, so a lock
// that misjudges a live run costs a delay rather than a running job's credentials.
//
// The root the directories live under is chosen and proved before a process serves: see ChooseRoot
// and Prepare. Every server and worker sweeps its root at startup and every few minutes after, with
// a lock in the root so one process per host sweeps a given pass.
package runfiles

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"time"
)

const (
	// lockName is the lock file inside every run directory.
	lockName = ".lock"
	// dirPrefix starts every run directory's name, so a sweep touches nothing else under the root.
	dirPrefix = "run-"
	// maxLabelLen bounds the label part of a run directory's name.
	maxLabelLen = 48
)

// labelUnsafe matches what may not appear in the label part of a directory name.
var labelUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// Dir is one run's private directory. Its owner holds the lock inside it and keeps its heartbeat
// counter moving until Remove.
type Dir struct {
	// path is the directory's absolute path.
	path string
	// lock is the open, locked lock file that marks the directory as live.
	lock *os.File
	// beat increments the directory's heartbeat counter until the directory is removed.
	beat *heartbeat
}

// DefaultRoot returns the directory run directories are created under when nothing better is
// available: a per-account directory in the system temporary directory, so two accounts on one host
// never share one. ChooseRoot prefers a memory-backed runtime directory when the host offers one.
func DefaultRoot() string {
	name := "switchtender-runfiles"
	if uid := os.Getuid(); uid >= 0 {
		name += "-" + strconv.Itoa(uid)
	}
	return filepath.Join(os.TempDir(), name)
}

// Create makes a new private run directory under root, labeled for the run it serves, locks it, and
// starts its heartbeat. The root is created mode 0700 when missing, and refused when it is a
// symbolic link, not a directory, or owned by another account, since any of those would let someone
// else read or swap what a run writes.
func Create(root, label string) (*Dir, error) {
	return create(root, label, BeatInterval)
}

// create is Create with the heartbeat interval chosen by the caller.
func create(root, label string, every time.Duration) (*Dir, error) {
	if err := prepareRoot(root); err != nil {
		return nil, err
	}
	label = labelUnsafe.ReplaceAllString(label, "_")
	if len(label) > maxLabelLen {
		label = label[:maxLabelLen]
	}
	// A sweep can win the lock in the moment between the lock file existing and this process locking
	// it, and remove the directory. That is detected below, and a fresh directory is made instead.
	for range 3 {
		path, err := os.MkdirTemp(root, dirPrefix+label+"-")
		if err != nil {
			return nil, fmt.Errorf("create run directory: %w", err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			_ = removeTree(path)
			return nil, fmt.Errorf("create run directory: %w", err)
		}
		lock, err := createLockFile(filepath.Join(path, lockName))
		if err != nil {
			_ = removeTree(path)
			return nil, fmt.Errorf("create run directory lock: %w", err)
		}
		if _, err := lockFile(lock, true); err != nil {
			_ = lock.Close()
			_ = removeTree(path)
			return nil, fmt.Errorf("lock run directory: %w", err)
		}
		if !stillThere(lock, path) {
			_ = unlockFile(lock)
			_ = lock.Close()
			continue
		}
		beat, err := startHeartbeat(path, every)
		if err != nil {
			_ = removeLocked(path, lock)
			return nil, fmt.Errorf("start run directory heartbeat: %w", err)
		}
		return &Dir{path: path, lock: lock, beat: beat}, nil
	}
	return nil, fmt.Errorf("%w: a sweep removed every run directory as it was created", ErrCreate)
}

// Path returns the directory's absolute path.
func (d *Dir) Path() string { return d.path }

// CreateTemp creates a new file mode 0600 in the directory, named from pattern the way
// os.CreateTemp names one. The caller closes it.
func (d *Dir) CreateTemp(pattern string) (*os.File, error) {
	f, err := os.CreateTemp(d.path, pattern)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	return f, nil
}

// WriteFile writes content to a new file mode 0600 in the directory and returns its path.
func (d *Dir) WriteFile(pattern, content string) (string, error) {
	f, err := d.CreateTemp(pattern)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// Remove stops the heartbeat, deletes the directory and everything in it, and releases its lock. It
// is safe to call more than once and on a nil Dir.
func (d *Dir) Remove() error {
	if d == nil || d.lock == nil {
		return nil
	}
	d.beat.halt()
	d.beat = nil
	err := removeLocked(d.path, d.lock)
	d.lock = nil
	return err
}

// removeLocked deletes a run directory whose lock the caller holds through lock, and releases the
// lock once the directory is gone.
func removeLocked(path string, lock *os.File) error {
	return removeHeld(path, func() {
		_ = unlockFile(lock)
		_ = lock.Close()
	})
}

// removeHeld deletes a run directory whose lock the caller holds, and calls release to give the
// lock up. Everything but the lock file goes first, so no other process can see a directory with
// secret material in it and no lock. The lock file and the directory go next, before the release. A
// Create that made its lock file and stalled before locking it takes the lock the moment it is
// released, and had its lock file still been at its path then, it would have kept a directory
// deleted a moment later.
func removeHeld(path string, release func()) error {
	var errs []error
	entries, err := os.ReadDir(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	for _, e := range entries {
		if e.Name() == lockName {
			continue
		}
		if err := removeTree(filepath.Join(path, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	if runtime.GOOS == "windows" {
		// Windows refuses to delete a file that is still open, so there the lock file and the
		// directory go after the release.
		release()
		if err := removeTree(path); err != nil {
			errs = append(errs, err)
		}
	} else {
		if err := removeTree(path); err != nil {
			errs = append(errs, err)
		}
		release()
	}
	if len(errs) > 0 {
		return fmt.Errorf("remove run directory: %w", errors.Join(errs...))
	}
	return nil
}

// removeTree deletes path and everything below it. A tool can leave directories it made read-only,
// as Go's module cache does, and those refuse to give up their entries, and Windows refuses to
// delete a read-only file. So when the first attempt fails, every directory and regular file in the
// tree is made writable by its owner and the removal runs once more. Symbolic links are left as
// they are, since changing the mode of one changes whatever it points at. Everything in a run
// directory belongs to the account that removes it, so that suffices unless something else is
// wrong, which the returned error then says.
func removeTree(path string) error {
	err := os.RemoveAll(path)
	if err == nil {
		return nil
	}
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, werr error) error {
		switch {
		case werr != nil:
		case d.IsDir():
			_ = os.Chmod(p, 0o700)
		case d.Type().IsRegular():
			_ = os.Chmod(p, 0o600)
		}
		return nil
	})
	return os.RemoveAll(path)
}

// Sweep runs one pass of this process's sweeper for root and returns how many run directories it
// removed. A directory is removed only once the sweeper has seen it dead twice, at least
// ObservationGap apart, so a process that calls Sweep once at startup records what it found and
// removes it on a later call. A missing root has nothing to sweep.
func Sweep(root string) (int, error) {
	res, err := sweeperFor(root).Pass()
	return res.Removed, err
}

// createLockFile creates a run directory's lock file. Create and the startup probe both open it
// this way, so the probe proves the open mode a run actually uses.
func createLockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
}

// openLockFile opens an existing lock file as a second handle, the way a sweep does. The startup
// probe opens its second handle this way too.
func openLockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR, 0)
}

// stillThere reports whether the lock file the caller opened is still the one at its path, which is
// false when a sweep removed the directory before the caller locked it.
func stillThere(lock *os.File, path string) bool {
	held, err := lock.Stat()
	if err != nil {
		return false
	}
	now, err := os.Stat(filepath.Join(path, lockName))
	if err != nil {
		return false
	}
	return os.SameFile(held, now)
}

// prepareRoot creates root mode 0700 when it is missing and confirms it is safe to write secrets
// under: a real directory, not a symbolic link, owned by this account, and closed to everyone else.
func prepareRoot(root string) error {
	info, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return fmt.Errorf("create run directory root: %w", err)
		}
		info, err = os.Lstat(root)
	}
	if err != nil {
		return fmt.Errorf("create run directory root: %w", err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrUnsafeRoot, root)
	}
	if err := checkOwner(info); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUnsafeRoot, root, err)
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(root, 0o700); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrUnsafeRoot, root, err)
		}
	}
	return nil
}
