package project

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newBrowseSyncer returns a Syncer whose cache holds one checkout named proj_1, seeded with the
// given relative files.
func newBrowseSyncer(t *testing.T, files map[string]string) (*Syncer, string) {
	t.Helper()
	cache := t.TempDir()
	checkout := filepath.Join(cache, "proj_1")
	for rel, body := range files {
		full := filepath.Join(checkout, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	}
	s, err := NewSyncer(cache)
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	return s, checkout
}

// TestBrowseTree verifies listing skips the git directory and reports sizes.
func TestBrowseTree(t *testing.T) {
	t.Parallel()
	s, _ := newBrowseSyncer(t, map[string]string{
		"site.yml":           "- hosts: all\n",
		"roles/web/main.yml": "- name: install\n",
		".git/config":        "[remote]\n\turl = https://token@example.com/repo.git\n",
	})
	entries, err := s.Tree("proj_1")
	if err != nil {
		t.Fatalf("Tree() error = %v", err)
	}
	var paths []string
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	got := strings.Join(paths, ",")
	if got != "roles/web/main.yml,site.yml" {
		t.Errorf("Tree() = %q, want the two tracked files without the git directory", got)
	}
}

// TestBrowseFile verifies reading, traversal refusal, git refusal, and binary detection.
func TestBrowseFile(t *testing.T) {
	t.Parallel()
	s, checkout := newBrowseSyncer(t, map[string]string{
		"site.yml":    "- hosts: all\n",
		".git/config": "[remote]\n",
		"logo.bin":    string([]byte{0x00, 0x01, 0x02, 0xff}),
	})
	// A secret beside the cache, the file a traversal would try to reach.
	secret := filepath.Join(filepath.Dir(checkout), "secret.txt")
	if err := os.WriteFile(secret, []byte("do not leak"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	// A symlink inside the checkout pointing at that secret.
	if err := os.Symlink(secret, filepath.Join(checkout, "escape.txt")); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}

	tests := []struct {
		Path        string
		WantErr     error
		WantContent string
		WantBinary  bool
	}{
		{Path: "site.yml", WantContent: "- hosts: all\n"},
		{Path: "./site.yml", WantContent: "- hosts: all\n"},
		{Path: "../secret.txt", WantErr: ErrNotAFile},
		{Path: "../../etc/passwd", WantErr: ErrNotAFile},
		{Path: "roles/../site.yml", WantContent: "- hosts: all\n"},
		{Path: ".git/config", WantErr: ErrOutsideCheckout},
		{Path: "escape.txt", WantErr: ErrOutsideCheckout},
		{Path: "missing.yml", WantErr: ErrNotAFile},
		{Path: "logo.bin", WantBinary: true},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := s.File("proj_1", test.Path)
			if test.WantErr != nil {
				if err == nil {
					t.Fatalf("File(%q) = %+v, want error %v", test.Path, got, test.WantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("File(%q) error = %v", test.Path, err)
			}
			if got.Binary != test.WantBinary {
				t.Errorf("binary = %v, want %v", got.Binary, test.WantBinary)
			}
			if got.Content != test.WantContent {
				t.Errorf("content = %q, want %q", got.Content, test.WantContent)
			}
		})
	}
}

// TestBrowseNoCheckout verifies an unsynced project reports that rather than an empty listing.
func TestBrowseNoCheckout(t *testing.T) {
	t.Parallel()
	s, _ := newBrowseSyncer(t, nil)
	if _, err := s.Tree("never_synced"); err == nil {
		t.Error("Tree() on an unsynced project = nil error, want ErrNoCheckout")
	}
	// A project id carrying a separator must never resolve outside the cache.
	if _, err := s.File("../..", "site.yml"); err == nil {
		t.Error("File() with a traversing project id = nil error, want a refusal")
	}
}
