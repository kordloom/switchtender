//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package ansibleruntime

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// checkOwner refuses info unless it belongs to this account or to root, and, for anything but a
// symlink, unless no group or other account may write it. A symlink's own mode means nothing and
// only its owner is checked, since replacing it needs write permission on its directory, which is
// checked on its own. A directory with the sticky bit set that root owns is allowed to be writable
// when sticky is set, as /tmp is: nobody but an entry's owner can rename or remove the entry.
func checkOwner(path string, info fs.FileInfo, sticky bool) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%w: %s: the platform does not report its owner", ErrUntrusted, path)
	}
	uid := int(st.Uid)
	if uid != os.Geteuid() && uid != 0 {
		return fmt.Errorf("%w: %s is owned by uid %d, not by this account or root", ErrUntrusted,
			path, uid)
	}
	if info.Mode()&fs.ModeSymlink != 0 || info.Mode().Perm()&0o022 == 0 {
		return nil
	}
	if sticky && info.IsDir() && info.Mode()&fs.ModeSticky != 0 && uid == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s is writable by accounts other than its owner (mode %s)",
		ErrUntrusted, path, info.Mode().Perm())
}

// checkChain refuses dir unless it and every directory above it, with symlinks resolved, belongs
// to this account or root and no other account can write it, the way ssh's StrictModes checks a
// key's path. Only an ancestor may be a sticky directory root owns, such as /tmp.
func checkChain(dir string) error {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUntrusted, dir, err)
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUntrusted, dir, err)
	}
	for p, first := real, true; ; p, first = filepath.Dir(p), false {
		info, err := os.Lstat(p)
		if err != nil {
			return fmt.Errorf("%w: %s: %w", ErrUntrusted, p, err)
		}
		if err := checkOwner(p, info, !first); err != nil {
			return err
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
}

// checkFile refuses path unless it is a regular file, not a symlink, that belongs to this account
// or root and no other account can write.
func checkFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUntrusted, path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrUntrusted, path)
	}
	return checkOwner(path, info, false)
}

// checkTree refuses root unless every file and directory under it, itself included, belongs to
// this account or root and no other account can write it.
func checkTree(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("%w: %s: %w", ErrUntrusted, path, err)
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("%w: %s: %w", ErrUntrusted, path, err)
		}
		return checkOwner(path, info, false)
	})
}

// clearGroupOtherWrite removes the group and other write bits from everything under root, so an
// environment built under a permissive umask is not one another account can change.
func clearGroupOtherWrite(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o022 == 0 {
			return nil
		}
		return os.Chmod(path, info.Mode().Perm()&^0o022)
	})
}

// holdEnv takes a lock on the environment env's marker, shared for a run using it and exclusive
// for a remove, without waiting, and returns the function that releases it. It returns ErrInUse
// when the lock is held the other way. The marker is opened for reading only, so a run can hold an
// environment that it cannot write.
func holdEnv(env string, exclusive bool) (func(), error) {
	f, err := os.OpenFile(filepath.Join(env, markerName), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_SH | syscall.LOCK_NB
	if exclusive {
		how = syscall.LOCK_EX | syscall.LOCK_NB
	}
	for {
		err = syscall.Flock(int(f.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			if exclusive {
				return nil, fmt.Errorf("%w: a run is using %s", ErrInUse, env)
			}
			return nil, fmt.Errorf("%w: %s is being removed", ErrInUse, env)
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
