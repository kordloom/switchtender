package project

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// MaxFileBytes is the largest file the browser returns. Larger files are reported with their size
// and no content, so a browser request can never pull a multi-gigabyte artifact into memory.
const MaxFileBytes = 512 * 1024

// maxTreeEntries caps how many paths a listing returns, so a repository with a huge tree cannot
// stall the interface.
const maxTreeEntries = 5000

var (
	// ErrNoCheckout is returned when a project has never been synced, so nothing is on disk yet.
	ErrNoCheckout = errors.New("project has no local checkout yet")
	// ErrNotAFile is returned when a path resolves to a directory rather than a readable file.
	ErrNotAFile = errors.New("path is not a file")
	// ErrOutsideCheckout is returned when a path escapes the project's checkout directory.
	ErrOutsideCheckout = errors.New("path is outside the project checkout")
)

// TreeEntry is one file in a project's checkout.
type TreeEntry struct {
	// Path is the file's slash-separated path relative to the checkout root.
	Path string `json:"path"`
	// Size is the file's size in bytes.
	Size int64 `json:"size"`
}

// FileContent is one file's readable content, or its size alone when it is too large or binary.
type FileContent struct {
	// Path is the file's slash-separated path relative to the checkout root.
	Path string `json:"path"`
	// Size is the file's size in bytes.
	Size int64 `json:"size"`
	// Content is the file's text, empty when Binary or Truncated left nothing readable.
	Content string `json:"content"`
	// Binary reports that the file is not valid text, so no content is returned.
	Binary bool `json:"binary,omitempty"`
	// Truncated reports that the file exceeded MaxFileBytes and was cut at that limit.
	Truncated bool `json:"truncated,omitempty"`
}

// checkoutRoot returns the resolved checkout directory for a project id, following symlinks so
// later containment checks compare fully resolved paths.
func (s *Syncer) checkoutRoot(projectID string) (string, error) {
	if projectID == "" || strings.ContainsAny(projectID, `/\`) || projectID == ".." {
		return "", ErrOutsideCheckout
	}
	dir := filepath.Join(s.cacheDir, projectID)
	resolved, err := filepath.EvalSymlinks(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNoCheckout
	}
	if err != nil {
		return "", fmt.Errorf("resolve checkout: %w", err)
	}
	return resolved, nil
}

// Tree lists the readable files in a project's checkout, sorted by path. The git directory is
// skipped: it holds no playbooks and can carry remote credentials in its config. Symlinks are
// listed only when they resolve inside the checkout, so a repository cannot publish host files.
func (s *Syncer) Tree(projectID string) ([]TreeEntry, error) {
	root, err := s.checkoutRoot(projectID)
	if err != nil {
		return nil, err
	}
	l := s.lock(projectID)
	l.Lock()
	defer l.Unlock()

	var out []TreeEntry
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable entry is skipped rather than failing the whole listing.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if len(out) >= maxTreeEntries {
			return fs.SkipAll
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		// A symlink is listed only when it still points inside the checkout.
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(path)
			if err != nil || !withinRoot(root, target) {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, TreeEntry{Path: filepath.ToSlash(rel), Size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk checkout: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// File returns one file from a project's checkout. The path is cleaned and confined to the
// checkout, symlinks are resolved before the containment check, the git directory is refused, and
// oversized or binary content is reported rather than returned.
func (s *Syncer) File(projectID, rel string) (*FileContent, error) {
	root, err := s.checkoutRoot(projectID)
	if err != nil {
		return nil, err
	}
	clean := filepath.Clean("/" + filepath.FromSlash(rel))
	full := filepath.Join(root, clean)
	if !withinRoot(root, full) {
		return nil, ErrOutsideCheckout
	}
	if parts := strings.Split(filepath.ToSlash(strings.TrimPrefix(clean, string(filepath.Separator))), "/"); len(parts) > 0 && parts[0] == ".git" {
		return nil, ErrOutsideCheckout
	}

	l := s.lock(projectID)
	l.Lock()
	defer l.Unlock()

	// Resolving after joining catches a symlink inside the checkout that points out of it.
	resolved, err := filepath.EvalSymlinks(full)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotAFile
	}
	if err != nil {
		return nil, fmt.Errorf("resolve path: %w", err)
	}
	if !withinRoot(root, resolved) {
		return nil, ErrOutsideCheckout
	}
	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() || !info.Mode().IsRegular() {
		return nil, ErrNotAFile
	}

	out := &FileContent{Path: filepath.ToSlash(strings.TrimPrefix(clean, string(filepath.Separator))), Size: info.Size()}
	f, err := os.Open(resolved)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, MaxFileBytes)
	n, err := f.Read(buf)
	if err != nil && n == 0 && info.Size() > 0 {
		return nil, fmt.Errorf("read file: %w", err)
	}
	buf = buf[:n]
	if !utf8.Valid(buf) || strings.ContainsRune(string(buf), 0) {
		out.Binary = true
		return out, nil
	}
	out.Content = string(buf)
	out.Truncated = info.Size() > int64(n)
	return out, nil
}

// withinRoot reports whether path is root itself or sits under it, comparing whole path elements
// so a sibling directory sharing a name prefix is not mistaken for a child.
func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}
