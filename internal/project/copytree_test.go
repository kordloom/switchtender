package project

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCopyTreePreservesModesAndLinks proves an isolated checkout carries executable bits and
// symlinks intact, and leaves out the skipped paths.
//
// A run executes what the copy contains, so a script that loses its executable bit will not run and
// a symlink followed into a file would change what a play reads. Both must survive the copy exactly.
func TestCopyTreePreservesModesAndLinks(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "play.yml"), "---\n", 0o644)
	writeFile(t, filepath.Join(src, "run.sh"), "#!/bin/sh\necho hi\n", 0o755)
	mkdir(t, filepath.Join(src, "roles"))
	writeFile(t, filepath.Join(src, "roles", "task.yml"), "- name: x\n", 0o644)
	mkdir(t, filepath.Join(src, ".git"))
	writeFile(t, filepath.Join(src, ".git", "config"), "[core]\n", 0o644)
	if err := os.Symlink("roles/task.yml", filepath.Join(src, "link.yml")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	dst := t.TempDir()
	if err := copyTree(src, dst, func(rel string) bool {
		return rel == ".git" || rel == ".git"+string(os.PathSeparator)+"config"
	}); err != nil {
		t.Fatalf("copyTree() error = %v", err)
	}

	// The executable bit survives.
	info, err := os.Lstat(filepath.Join(dst, "run.sh"))
	if err != nil {
		t.Fatalf("stat run.sh: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("run.sh lost its executable bit, mode = %v", info.Mode())
	}
	// The symlink is copied as a link, not dereferenced.
	linfo, err := os.Lstat(filepath.Join(dst, "link.yml"))
	if err != nil {
		t.Fatalf("stat link.yml: %v", err)
	}
	if linfo.Mode()&os.ModeSymlink == 0 {
		t.Errorf("link.yml was dereferenced into a regular file, mode = %v", linfo.Mode())
	}
	// The skipped .git tree is absent.
	if _, err := os.Stat(filepath.Join(dst, ".git")); !os.IsNotExist(err) {
		t.Errorf(".git was copied, err = %v", err)
	}
	// A nested regular file came across.
	if _, err := os.Stat(filepath.Join(dst, "roles", "task.yml")); err != nil {
		t.Errorf("nested file missing: %v", err)
	}
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}
