package dispatch

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/kordloom/switchtender/internal/inventory"
)

// Bounds on how much of a file-path inventory is read for its secrets, so a run pointed at a large
// directory cannot stall its own start.
const (
	// maxInventoryScanBytes is the most read from one inventory file.
	maxInventoryScanBytes = 4 << 20
	// maxInventoryScanFiles is the most files an inventory directory contributes.
	maxInventoryScanFiles = 256
)

// pathInventorySecrets returns the secret-looking values in an inventory named by path rather than
// stored: a hosts file, or a directory holding hosts files, group_vars, and host_vars.
//
// A stored inventory was always scanned, so an ansible_password or an api_token in it was masked in
// the run's output. The same host list kept as a file, beside the server or inside a project, was
// never read, and its secrets printed in clear in the log, the events, and the live stream. It is
// read with the same detection now. An executable file is a dynamic inventory script, whose output
// rather than its text holds the hosts, so it is left alone, as are symbolic links.
func pathInventorySecrets(path string) []string {
	if path == "" {
		return nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil
	}
	if info.Mode().IsRegular() {
		if info.Mode()&0o111 != 0 {
			return nil
		}
		return inventoryFileSecrets(path)
	}
	if !info.IsDir() {
		return nil
	}
	var out []string
	files := 0
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if files >= maxInventoryScanFiles {
			return fs.SkipAll
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 != 0 {
			return nil
		}
		files++
		out = append(out, inventoryFileSecrets(p)...)
		return nil
	})
	return out
}

// inventoryFileSecrets returns the secret-looking values in one inventory file.
func inventoryFileSecrets(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(io.LimitReader(f, maxInventoryScanBytes))
	if err != nil {
		return nil
	}
	return inventory.Secrets(string(body))
}
