package project

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// maxLinkHops is how many symbolic links one path may pass through before it is refused, the bound
// Linux applies.
const maxLinkHops = 40

// maxLinkBytes is the longest symbolic link target read, the longest path Linux accepts.
const maxLinkBytes = 4096

// ReadCommit calls fn with a project's files as they stood at commit, or at the checkout's current
// commit when commit is empty, and the full hash that resolved to. It holds the project's sync lock
// throughout, so no sync can move or rewrite the checkout while fn reads it.
//
// The files come from the repository's history rather than its working tree, so a commit other
// than the current one reads exactly as it was committed: a run held for approval is read at the
// commit it is pinned to after the branch has moved on, and a finished run at the commit it
// executed. The Ansible dependencies a sync installs are not history, so they are read from the
// checkout's dependency directory as the last sync left it.
//
// It is safe to call on a nil Syncer, which has no checkouts.
func (s *Syncer) ReadCommit(projectID, commit string, fn func(fsys fs.FS, sha string) error) error {
	if s == nil {
		return ErrNoCheckout
	}
	root, err := s.checkoutRoot(projectID)
	if err != nil {
		return err
	}
	l := s.lock(projectID)
	l.Lock()
	defer l.Unlock()

	repo, err := git.PlainOpen(root)
	if err != nil {
		return fmt.Errorf("open checkout: %w", err)
	}
	var hash plumbing.Hash
	switch {
	case commit == "":
		head, herr := repo.Head()
		if herr != nil {
			return fmt.Errorf("resolve head: %w", herr)
		}
		hash = head.Hash()
	case plumbing.IsHash(commit):
		hash = plumbing.NewHash(commit)
	default:
		return fmt.Errorf("%w: %q is not a commit hash", ErrNoCommit, commit)
	}
	c, err := repo.CommitObject(hash)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrNoCommit, hash, err)
	}
	tree, err := c.Tree()
	if err != nil {
		return fmt.Errorf("read commit tree: %w", err)
	}
	fsys := fs.FS(&commitFS{repo: repo, root: tree})
	if deps, derr := os.OpenRoot(filepath.Join(root, galaxyDir)); derr == nil {
		defer func() { _ = deps.Close() }()
		fsys = overlayFS{base: fsys, prefix: galaxyDir, over: deps.FS()}
	}
	return fn(fsys, hash.String())
}

// commitFS serves the files of one commit as an fs.FS. A symbolic link is followed inside the
// commit and refused when it points out of it, so a repository cannot use a link to have a file on
// the machine read as though it were its own.
type commitFS struct {
	// repo is the repository the objects are read from.
	repo *git.Repository
	// root is the commit's top-level tree.
	root *object.Tree
}

// treeEntry is what a path in a commit resolved to.
type treeEntry struct {
	// hash is the entry's object.
	hash plumbing.Hash
	// dir reports that the entry is a directory.
	dir bool
}

// Open opens the file or directory at name, following symbolic links.
func (c *commitFS) Open(name string) (fs.File, error) {
	entry, err := c.resolve("open", name)
	if err != nil {
		return nil, err
	}
	if entry.dir {
		return &commitDir{info: dirInfo(name)}, nil
	}
	blob, err := c.repo.BlobObject(entry.hash)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	body, err := blob.Reader()
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return &commitFile{body: body, info: fileInfo{name: path.Base(name), size: blob.Size,
		mode: 0o444}}, nil
}

// Stat describes the file or directory at name, following symbolic links.
func (c *commitFS) Stat(name string) (fs.FileInfo, error) {
	entry, err := c.resolve("stat", name)
	if err != nil {
		return nil, err
	}
	if entry.dir {
		return dirInfo(name), nil
	}
	blob, err := c.repo.BlobObject(entry.hash)
	if err != nil {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
	}
	return fileInfo{name: path.Base(name), size: blob.Size, mode: 0o444}, nil
}

// resolve walks name through the commit and returns the entry it ends at, following symbolic links.
func (c *commitFS) resolve(op, name string) (treeEntry, error) {
	if !fs.ValidPath(name) {
		return treeEntry{}, &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	current := name
	for hops := 0; hops <= maxLinkHops; hops++ {
		entry, next, err := c.walk(current)
		if err != nil {
			return treeEntry{}, &fs.PathError{Op: op, Path: name, Err: err}
		}
		if next == "" {
			return entry, nil
		}
		current = next
	}
	return treeEntry{}, &fs.PathError{Op: op, Path: name, Err: errTooManyLinks}
}

// walk follows current through the commit one element at a time. It returns the entry the path
// ends at, or, when the path passes through a symbolic link, the path rewritten through that link
// to walk next. A link is resolved against the directory holding it, and a result that leaves the
// commit is refused.
func (c *commitFS) walk(current string) (treeEntry, string, error) {
	if current == "." {
		return treeEntry{hash: c.root.Hash, dir: true}, "", nil
	}
	parts := strings.Split(current, "/")
	tree := c.root
	for i, part := range parts {
		entry, err := tree.FindEntry(part)
		if err != nil {
			return treeEntry{}, "", fs.ErrNotExist
		}
		last := i == len(parts)-1
		switch entry.Mode {
		case filemode.Symlink:
			target, err := c.linkTarget(entry.Hash)
			if err != nil {
				return treeEntry{}, "", err
			}
			next := path.Join(path.Join(parts[:i]...), target, path.Join(parts[i+1:]...))
			if path.IsAbs(target) || !fs.ValidPath(next) {
				return treeEntry{}, "", ErrOutsideCheckout
			}
			return treeEntry{}, next, nil
		case filemode.Dir:
			if last {
				return treeEntry{hash: entry.Hash, dir: true}, "", nil
			}
			if tree, err = c.repo.TreeObject(entry.Hash); err != nil {
				return treeEntry{}, "", err
			}
		case filemode.Regular, filemode.Executable, filemode.Deprecated:
			if last {
				return treeEntry{hash: entry.Hash}, "", nil
			}
			return treeEntry{}, "", fs.ErrNotExist
		default:
			// A submodule is another repository, whose files this commit does not hold.
			return treeEntry{}, "", fs.ErrNotExist
		}
	}
	return treeEntry{}, "", fs.ErrNotExist
}

// linkTarget returns the path a symbolic link stored in the commit points at.
func (c *commitFS) linkTarget(hash plumbing.Hash) (string, error) {
	blob, err := c.repo.BlobObject(hash)
	if err != nil {
		return "", err
	}
	body, err := blob.Reader()
	if err != nil {
		return "", err
	}
	defer func() { _ = body.Close() }()
	target, err := io.ReadAll(io.LimitReader(body, maxLinkBytes+1))
	if err != nil {
		return "", err
	}
	if len(target) > maxLinkBytes {
		return "", errTooManyLinks
	}
	return string(target), nil
}

// overlayFS serves the paths under prefix from over and every other path from base, so the
// dependencies a sync installs read beside the files a commit holds.
type overlayFS struct {
	// base serves every path outside prefix.
	base fs.FS
	// prefix is the directory over serves.
	prefix string
	// over serves prefix and everything under it.
	over fs.FS
}

// route returns the filesystem that serves name and the name within it.
func (o overlayFS) route(name string) (fs.FS, string) {
	if name == o.prefix {
		return o.over, "."
	}
	if rest, ok := strings.CutPrefix(name, o.prefix+"/"); ok {
		return o.over, rest
	}
	return o.base, name
}

// Open opens name from whichever filesystem serves it.
func (o overlayFS) Open(name string) (fs.File, error) {
	fsys, rel := o.route(name)
	return fsys.Open(rel)
}

// Stat describes name from whichever filesystem serves it.
func (o overlayFS) Stat(name string) (fs.FileInfo, error) {
	fsys, rel := o.route(name)
	return fs.Stat(fsys, rel)
}

// commitFile is an open file from a commit.
type commitFile struct {
	// body streams the file's content.
	body io.ReadCloser
	// info describes the file.
	info fileInfo
}

// Stat describes the file.
func (f *commitFile) Stat() (fs.FileInfo, error) { return f.info, nil }

// Read reads the file's content.
func (f *commitFile) Read(p []byte) (int, error) { return f.body.Read(p) }

// Close releases the file.
func (f *commitFile) Close() error { return f.body.Close() }

// commitDir is an open directory from a commit. It can be described but not read.
type commitDir struct {
	// info describes the directory.
	info fileInfo
}

// Stat describes the directory.
func (d *commitDir) Stat() (fs.FileInfo, error) { return d.info, nil }

// Read refuses, since a directory has no content to read.
func (d *commitDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.info.name, Err: fs.ErrInvalid}
}

// Close releases the directory.
func (d *commitDir) Close() error { return nil }

// fileInfo describes a file or directory in a commit.
type fileInfo struct {
	// name is the base name.
	name string
	// size is the content length in bytes, zero for a directory.
	size int64
	// mode carries the directory bit and read permissions.
	mode fs.FileMode
}

// dirInfo describes the directory at name.
func dirInfo(name string) fileInfo {
	return fileInfo{name: path.Base(name), mode: fs.ModeDir | 0o555}
}

// Name returns the base name.
func (i fileInfo) Name() string { return i.name }

// Size returns the content length in bytes.
func (i fileInfo) Size() int64 { return i.size }

// Mode returns the mode bits.
func (i fileInfo) Mode() fs.FileMode { return i.mode }

// ModTime returns the zero time, since a commit's files carry no modification time of their own.
func (i fileInfo) ModTime() time.Time { return time.Time{} }

// IsDir reports whether the entry is a directory.
func (i fileInfo) IsDir() bool { return i.mode.IsDir() }

// Sys returns nil.
func (i fileInfo) Sys() any { return nil }
