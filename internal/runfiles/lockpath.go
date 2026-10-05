package runfiles

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LockPath takes an exclusive lock on the file at path, creating it mode 0600 when it is missing,
// and waits while another holder has it. It returns the release. The lock belongs to the open file,
// so two holders in one process exclude each other the way two processes do, and the operating
// system drops it when its holder exits, however it exits.
func LockPath(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	if _, err := lockFile(f, true); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("take lock: %w", err)
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}

// RemoveDead deletes the run directories under root whose owners are gone and returns how many it
// removed. An owner holds its directory's lock for as long as it lives, and the operating system
// releases the lock when the owner dies, so a directory whose lock this call can take belongs to
// nobody. A directory whose lock is held is left alone, which is what keeps a process starting on a
// root that another live process uses from deleting that process's work. A directory with no lock
// file is being created at this moment, or its creator died creating it, so it is removed only once
// its entries have gone MinAge without a change.
//
// It decides on one observation, where Sweep waits for two. It is for directories whose liveness is
// the lock and nothing else, such as a run's private project checkout. A run's secret files keep
// the two-observation sweep.
func RemoveDead(root string) (int, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("remove dead run directories: %w", err)
	}
	removed := 0
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), dirPrefix) {
			continue
		}
		gone, err := removeIfDead(filepath.Join(root, e.Name()))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
		}
		if gone {
			removed++
		}
	}
	if len(errs) > 0 {
		return removed, fmt.Errorf("remove dead run directories: %w", errors.Join(errs...))
	}
	return removed, nil
}

// removeIfDead deletes the run directory at path when its owner is gone, and reports whether it
// did.
func removeIfDead(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// A link is not something Create makes, so it is neither followed nor removed.
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return false, nil
	}
	lock, err := openLockFile(filepath.Join(path, lockName))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// A time in the future counts as old, as it does for the sweep, since only a clock stepped
		// backward produces one.
		if age := time.Since(info.ModTime()); age >= 0 && age < MinAge {
			return false, nil
		}
		if err := removeTree(path); err != nil {
			return false, err
		}
		return true, nil
	case err != nil:
		return false, err
	}
	free, err := lockFile(lock, false)
	if err != nil || !free {
		_ = lock.Close()
		return false, err
	}
	if err := removeLocked(path, lock); err != nil {
		return false, err
	}
	return true, nil
}
