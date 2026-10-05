package roundhouse

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// secretsMount returns the --tmpfs argument that makes a run's private directory an in-memory
// filesystem inside its container, at the path the directory has on the host so every path the
// run's environment names resolves the same in both.
//
// The size is explicit, because an unsized tmpfs may grow to half the host's memory and everything
// in it counts against the container's memory limit. nosuid and nodev keep a file placed there from
// gaining privileges or reaching a device. exec stays allowed, which a container runtime turns off
// for a tmpfs unless told otherwise, so nothing a tool keeps there is barred from running. When the
// run executes as this account's uid the mount belongs to it alone. Otherwise it is open the way
// /tmp is, which inside a container reaches no one but the run itself.
func (c *containerRunner) secretsMount(dir string) (string, error) {
	if !strings.HasPrefix(dir, "/") || strings.Contains(dir, ":") {
		return "", fmt.Errorf("the run directory %s is not a path a container can mount its "+
			"in-memory secrets directory at", dir)
	}
	size := c.limits.RunFilesSize
	if size == "" {
		size = DefaultRunFilesSize
	}
	opts := []string{"rw", "exec", "nosuid", "nodev", "size=" + size}
	if uid := os.Getuid(); uid >= 0 {
		opts = append(opts, "mode=0700", "uid="+strconv.Itoa(uid), "gid="+strconv.Itoa(os.Getgid()))
	} else {
		opts = append(opts, "mode=1777")
	}
	return dir + ":" + strings.Join(opts, ","), nil
}

// insidePrivateRoot reports whether path lies strictly inside root, the run-files root, through no
// symbolic link, and root is a real directory this account owns with no access for anybody else. A
// path like that holds only what SwitchTender staged for a run, so the mount guard, which exists to
// keep a run away from the host's own secrets, has nothing to refuse there even when the root sits
// in a tree it otherwise refuses, such as a systemd runtime directory under /run. A socket is still
// refused, since nothing staged for a run is one.
func insidePrivateRoot(root, path string) bool {
	if root == "" || !filepath.IsAbs(root) || !filepath.IsAbs(path) {
		return false
	}
	root, path = filepath.Clean(root), filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	up := ".." + string(filepath.Separator)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, up) {
		return false
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 ||
		info.Mode().Perm()&0o077 != 0 || !ownedBySelf(info) {
		return false
	}
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil || fi.Mode()&fs.ModeSymlink != 0 {
			return false
		}
	}
	return !strings.HasSuffix(strings.ToLower(path), ".sock")
}
