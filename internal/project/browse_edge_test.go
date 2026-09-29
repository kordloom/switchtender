package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// seedCheckout writes files into a fresh cache under the given project id and returns the syncer
// and the checkout path. It is the browse fixture for cases that need more than a flat file list.
func seedCheckout(t *testing.T, projectID string, files map[string]string) (*Syncer, string) {
	t.Helper()
	cache := t.TempDir()
	checkout := filepath.Join(cache, projectID)
	if err := os.MkdirAll(checkout, 0o750); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	for rel, body := range files {
		full := filepath.Join(checkout, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", rel, err)
		}
	}
	s, err := NewSyncer(cache)
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	return s, checkout
}

// TestCheckoutRootRefusesATraversingProjectID pins the guard on the one value that names a directory
// under the cache. The project id becomes a path element, so a value carrying a separator or a
// parent reference would resolve the browse endpoints anywhere on the host filesystem.
func TestCheckoutRootRefusesATraversingProjectID(t *testing.T) {
	t.Parallel()
	s, _ := seedCheckout(t, "proj_1", map[string]string{"site.yml": "---\n"})
	tests := []struct {
		// In is the project id the caller supplied.
		In string
		// Want is the error class expected.
		Want error
	}{
		{In: "", Want: ErrOutsideCheckout},          // Test 0: Empty.
		{In: "..", Want: ErrOutsideCheckout},        // Test 1: The parent directory.
		{In: "../..", Want: ErrOutsideCheckout},     // Test 2: Deeper traversal.
		{In: "../proj_1", Want: ErrOutsideCheckout}, // Test 3: Traversal back to a real id.
		{In: "a/b", Want: ErrOutsideCheckout},       // Test 4: A forward slash.
		{In: `a\b`, Want: ErrOutsideCheckout},       // Test 5: A backslash.
		{In: "/etc", Want: ErrOutsideCheckout},      // Test 6: An absolute path.
		{In: "proj_missing", Want: ErrNoCheckout},   // Test 7: An id that was never synced.
		{In: "proj_1", Want: nil},                   // Test 8: The real checkout.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			root, err := s.checkoutRoot(test.In)
			if !errors.Is(err, test.Want) {
				t.Fatalf("checkoutRoot(%q) = (%q, %v), want %v", test.In, root, err, test.Want)
			}
			if test.Want == nil && !strings.HasSuffix(root, "proj_1") {
				t.Errorf("checkoutRoot(%q) = %q, want the project's own checkout", test.In, root)
			}
		})
	}
}

// TestWithinRootComparesWholeElements pins the containment helper both browse entry points use. A
// prefix comparison on raw strings would treat a sibling directory sharing a name prefix as a child,
// which is the classic way a containment check lets a neighbor's files through.
func TestWithinRootComparesWholeElements(t *testing.T) {
	t.Parallel()
	root := filepath.FromSlash("/srv/cache/proj_1")
	tests := []struct {
		// Path is the candidate path.
		Path string
		// WantWithin is whether the path is the root or sits under it.
		WantWithin bool
	}{
		{Path: "/srv/cache/proj_1", WantWithin: true},              // Test 0: The root itself.
		{Path: "/srv/cache/proj_1/site.yml", WantWithin: true},     // Test 1: A direct child.
		{Path: "/srv/cache/proj_1/a/b/c.yml", WantWithin: true},    // Test 2: A deep child.
		{Path: "/srv/cache/proj_10/secret", WantWithin: false},     // Test 3: A name-prefix sibling.
		{Path: "/srv/cache/proj_1x", WantWithin: false},            // Test 4: Another prefix sibling.
		{Path: "/srv/cache", WantWithin: false},                    // Test 5: The parent.
		{Path: "/etc/passwd", WantWithin: false},                   // Test 6: Somewhere else entirely.
		{Path: "/srv/cache/proj_1/../proj_2/x", WantWithin: false}, // Test 7: Traversal out.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := withinRoot(root, filepath.Clean(filepath.FromSlash(test.Path)))
			if got != test.WantWithin {
				t.Errorf("withinRoot(%q, %q) = %v, want %v", root, test.Path, got, test.WantWithin)
			}
		})
	}
}

// TestBrowseFileBoundaries covers the size and content edges of the file reader. The browser is a
// read path exposed to anyone who may use a project, so an oversized artifact must be reported by
// size rather than pulled into memory, and content that is not text must be refused rather than
// returned as a string full of control bytes.
func TestBrowseFileBoundaries(t *testing.T) {
	t.Parallel()
	s, _ := seedCheckout(t, "proj_1", map[string]string{
		"empty.yml":        "",
		"one.yml":          "x",
		"unicode.yml":      "# ünïcödé ✓\n",
		"exact.bin":        strings.Repeat("a", MaxFileBytes),
		"over.bin":         strings.Repeat("a", MaxFileBytes+1),
		"nul.bin":          "text\x00more",
		"deep/a/b/c/d.yml": "---\n",
	})

	tests := []struct {
		// Path is the requested repository-relative path.
		Path string
		// WantSize is the size the reader must report.
		WantSize int64
		// WantContentLen is how many bytes of content come back.
		WantContentLen int
		// WantBinary is whether the file must be reported as not text.
		WantBinary bool
		// WantTruncated is whether the content was cut at the limit.
		WantTruncated bool
	}{{ // Test 0: An empty file reads as empty text, not as binary.
		Path: "empty.yml", WantSize: 0, WantContentLen: 0,
	}, { // Test 1: A single byte.
		Path: "one.yml", WantSize: 1, WantContentLen: 1,
	}, { // Test 2: Multi-byte text comes back whole.
		Path: "unicode.yml", WantSize: int64(len("# ünïcödé ✓\n")), WantContentLen: len("# ünïcödé ✓\n"),
	}, { // Test 3: A file exactly at the limit is returned whole and is not marked truncated.
		Path: "exact.bin", WantSize: MaxFileBytes, WantContentLen: MaxFileBytes,
	}, { // Test 4: One byte over the limit is cut and marked.
		Path: "over.bin", WantSize: MaxFileBytes + 1, WantContentLen: MaxFileBytes, WantTruncated: true,
	}, { // Test 5: A NUL byte makes the file binary even though the bytes are valid UTF-8.
		Path: "nul.bin", WantSize: int64(len("text\x00more")), WantBinary: true,
	}, { // Test 6: A deeply nested path resolves normally.
		Path: "deep/a/b/c/d.yml", WantSize: 4, WantContentLen: 4,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := s.File("proj_1", test.Path)
			if err != nil {
				t.Fatalf("File(%q) error = %v", test.Path, err)
			}
			if got.Size != test.WantSize {
				t.Errorf("size = %d, want %d", got.Size, test.WantSize)
			}
			if got.Binary != test.WantBinary {
				t.Errorf("binary = %v, want %v", got.Binary, test.WantBinary)
			}
			if got.Truncated != test.WantTruncated {
				t.Errorf("truncated = %v, want %v", got.Truncated, test.WantTruncated)
			}
			if len(got.Content) != test.WantContentLen {
				t.Errorf("content length = %d, want %d", len(got.Content), test.WantContentLen)
			}
			if got.Binary && got.Content != "" {
				t.Errorf("a binary file returned %d bytes of content", len(got.Content))
			}
		})
	}
}

// TestBrowseFileRefusals covers every shape that must not produce a file. The refusals are
// deliberately alike from the outside so a caller cannot map the host filesystem by probing, but
// each one has to happen: the git directory holds credential helper settings and remote URLs, and a
// path that leaves the checkout reads whatever the server process can.
func TestBrowseFileRefusals(t *testing.T) {
	t.Parallel()
	s, checkout := seedCheckout(t, "proj_1", map[string]string{
		"site.yml":        "---\n",
		".git/config":     "[remote]\n\turl = https://token@example.com/repo.git\n",
		"sub/.git/config": "[core]\n",
		"roles/main.yml":  "- name: x\n",
	})
	outside := filepath.Join(filepath.Dir(checkout), "secret.txt")
	if err := os.WriteFile(outside, []byte("do not leak"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(checkout, "escape.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(checkout, ".git"), filepath.Join(checkout, "gitdir")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink("/etc", filepath.Join(checkout, "etc")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	tests := []struct {
		// Path is the requested repository-relative path.
		Path string
		// Want is the error class expected.
		Want error
	}{
		{Path: ".git/config", Want: ErrOutsideCheckout},     // Test 0: The git directory.
		{Path: ".GIT/config", Want: ErrOutsideCheckout},     // Test 1: A case variant of it.
		{Path: "sub/.git/config", Want: ErrOutsideCheckout}, // Test 2: A submodule's git directory.
		{Path: "gitdir/config", Want: ErrOutsideCheckout},   // Test 3: A symlink to the git directory.
		{Path: "escape.txt", Want: ErrOutsideCheckout},      // Test 4: A symlink out of the checkout.
		{Path: "etc/hosts", Want: ErrOutsideCheckout},       // Test 5: A symlink to an absolute path.
		{Path: "../secret.txt", Want: ErrNotAFile},          // Test 6: Traversal to a sibling.
		// Test 7: Deep traversal is clamped inside, where it meets the committed etc symlink and is
		// refused once that link is resolved.
		{Path: "../../etc/passwd", Want: ErrOutsideCheckout},
		// Test 8: An absolute path is clamped the same way and meets the same refusal.
		{Path: "/etc/passwd", Want: ErrOutsideCheckout},
		{Path: "missing.yml", Want: ErrNotAFile}, // Test 9: A file that is not there.
		{Path: "roles", Want: ErrNotAFile},       // Test 10: A directory is not a file.
		{Path: "", Want: ErrNotAFile},            // Test 11: Empty names the root directory.
		{Path: ".", Want: ErrNotAFile},           // Test 12: Dot names the root directory.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := s.File("proj_1", test.Path)
			if !errors.Is(err, test.Want) {
				t.Fatalf("File(%q) = (%+v, %v), want %v", test.Path, got, err, test.Want)
			}
			if got != nil {
				t.Errorf("File(%q) returned content alongside its refusal: %+v", test.Path, got)
			}
		})
	}
}

// TestBrowseFileAcceptsPathsThatStayInside is the other half of the refusals: a browse that rejected
// ordinary paths would make the projects page useless, so the shapes a caller really sends have to
// keep working.
func TestBrowseFileAcceptsPathsThatStayInside(t *testing.T) {
	t.Parallel()
	s, checkout := seedCheckout(t, "proj_1", map[string]string{
		"site.yml":       "---\n",
		"roles/main.yml": "- name: x\n",
		".gitignore":     "*.retry\n",
	})
	if err := os.Symlink(filepath.Join(checkout, "site.yml"), filepath.Join(checkout, "alias.yml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	tests := []struct {
		// Path is the requested repository-relative path.
		Path string
		// WantPath is the normalized path reported back.
		WantPath string
		// WantContent is the file body.
		WantContent string
	}{
		{Path: "site.yml", WantPath: "site.yml", WantContent: "---\n"},                   // Test 0.
		{Path: "./site.yml", WantPath: "site.yml", WantContent: "---\n"},                 // Test 1.
		{Path: "/site.yml", WantPath: "site.yml", WantContent: "---\n"},                  // Test 2.
		{Path: "roles/../site.yml", WantPath: "site.yml", WantContent: "---\n"},          // Test 3.
		{Path: "roles/main.yml", WantPath: "roles/main.yml", WantContent: "- name: x\n"}, // Test 4.
		{Path: ".gitignore", WantPath: ".gitignore", WantContent: "*.retry\n"},           // Test 5.
		{Path: "alias.yml", WantPath: "alias.yml", WantContent: "---\n"},                 // Test 6.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := s.File("proj_1", test.Path)
			if err != nil {
				t.Fatalf("File(%q) error = %v", test.Path, err)
			}
			if diff := cmp.Diff(test.WantPath, got.Path); diff != "" {
				t.Errorf("path mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantContent, got.Content); diff != "" {
				t.Errorf("content mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestBrowseTreeBoundsAndSkips covers the listing's own limits. It caps how much it returns so a
// repository with a huge tree cannot stall the interface, it leaves out the git directory at any
// depth, and it lists a symlink only when the link still resolves inside the checkout.
func TestBrowseTreeBoundsAndSkips(t *testing.T) {
	t.Parallel()

	// Test 0: the git directory is skipped wherever it appears, and ordinary files are sorted.
	s, checkout := seedCheckout(t, "proj_1", map[string]string{
		"site.yml":           "---\n",
		"roles/web/main.yml": "- name: install\n",
		".git/config":        "[remote]\n",
		"sub/.git/config":    "[core]\n",
		".gitignore":         "*.retry\n",
	})
	outside := filepath.Join(filepath.Dir(checkout), "secret.txt")
	if err := os.WriteFile(outside, []byte("do not leak"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(checkout, "escape.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(checkout, "site.yml"), filepath.Join(checkout, "alias.yml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	entries, err := s.Tree("proj_1")
	if err != nil {
		t.Fatalf("Tree() error = %v", err)
	}
	var paths []string
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	want := []string{".gitignore", "alias.yml", "roles/web/main.yml", "site.yml"}
	if diff := cmp.Diff(want, paths, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("Tree() mismatch (-want +got):\n%s", diff)
	}
	for _, e := range entries {
		if e.Size < 0 {
			t.Errorf("Tree() reported a negative size for %s", e.Path)
		}
	}
}

// TestBrowseTreeCapsHugeRepositories pins the entry cap. Without it a repository with a million
// files would be walked, held in memory, and serialized into one response, which is a request any
// authorized caller could make repeatedly.
func TestBrowseTreeCapsHugeRepositories(t *testing.T) {
	t.Parallel()
	files := make(map[string]string, maxTreeEntries+50)
	for i := range maxTreeEntries + 50 {
		files[fmt.Sprintf("f%05d.yml", i)] = "---\n"
	}
	s, _ := seedCheckout(t, "proj_big", files)
	entries, err := s.Tree("proj_big")
	if err != nil {
		t.Fatalf("Tree() error = %v", err)
	}
	if len(entries) > maxTreeEntries {
		t.Errorf("Tree() returned %d entries, want at most %d", len(entries), maxTreeEntries)
	}
	if len(entries) != maxTreeEntries {
		t.Errorf("Tree() returned %d entries, want the cap of %d to be reached", len(entries), maxTreeEntries)
	}
}

// TestBrowseRefusesAnUnsyncedProject pins that a project with nothing on disk says so rather than
// answering with an empty listing, which would read as a repository containing no playbooks.
func TestBrowseRefusesAnUnsyncedProject(t *testing.T) {
	t.Parallel()
	s, _ := seedCheckout(t, "proj_1", map[string]string{"site.yml": "---\n"})

	if _, err := s.Tree("proj_never_synced"); !errors.Is(err, ErrNoCheckout) {
		t.Errorf("Tree() on an unsynced project error = %v, want ErrNoCheckout", err)
	}
	if _, err := s.File("proj_never_synced", "site.yml"); !errors.Is(err, ErrNoCheckout) {
		t.Errorf("File() on an unsynced project error = %v, want ErrNoCheckout", err)
	}
	if _, err := s.Tree(".."); !errors.Is(err, ErrOutsideCheckout) {
		t.Errorf("Tree(\"..\") error = %v, want ErrOutsideCheckout", err)
	}
	if _, err := s.File("..", "site.yml"); !errors.Is(err, ErrOutsideCheckout) {
		t.Errorf("File(\"..\") error = %v, want ErrOutsideCheckout", err)
	}
}

// TestBrowseConfinesADotProjectID demonstrates a containment gap in the browse guard.
//
// checkoutRoot refuses an empty id, an id carrying a separator, and "..", which is the guard saying
// a project id must name exactly one directory under the cache. It does not refuse ".", which joins
// to the cache directory itself, so the listing walks every project's checkout at once and the file
// reader will serve any file under any of them. The handler in front of it looks the id up in the
// project store first, so this is a defense in depth failure rather than a live route today, but it
// is the one check standing between a project id and the whole cache.
func TestBrowseConfinesADotProjectID(t *testing.T) {
	t.Parallel()
	s, checkout := seedCheckout(t, "proj_1", map[string]string{"site.yml": "---\n"})
	neighbor := filepath.Join(filepath.Dir(checkout), "proj_2")
	if err := os.MkdirAll(neighbor, 0o750); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(neighbor, "private.yml"), []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := s.checkoutRoot("."); !errors.Is(err, ErrOutsideCheckout) {
		t.Errorf("checkoutRoot(\".\") error = %v, want ErrOutsideCheckout", err)
	}
	if got, err := s.File(".", "proj_2/private.yml"); err == nil {
		t.Errorf("File(\".\", %q) = %+v, which is another project's file", "proj_2/private.yml", got)
	}
}

// TestBrowseFileReportsLargeTextAsTruncatedNotBinary demonstrates a reporting bug at the size limit.
//
// The reader takes the first MaxFileBytes bytes and calls the file binary when those bytes are not
// valid UTF-8. A text file whose cut lands in the middle of a multi-byte character therefore comes
// back with Binary set and no content, when what happened is exactly what Truncated describes. The
// field docs say Binary means the file is not valid text, so a perfectly ordinary large playbook is
// reported as something it is not, and the browser shows nothing for it.
func TestBrowseFileReportsLargeTextAsTruncatedNotBinary(t *testing.T) {
	t.Parallel()
	// One ASCII byte then two-byte characters, so the cut at MaxFileBytes splits the last one.
	body := "a" + strings.Repeat("é", MaxFileBytes)
	s, _ := seedCheckout(t, "proj_1", map[string]string{"big.yml": body})
	got, err := s.File("proj_1", "big.yml")
	if err != nil {
		t.Fatalf("File() error = %v", err)
	}
	if got.Binary {
		t.Error("a UTF-8 text file was reported as binary because the cut split a character")
	}
	if !got.Truncated {
		t.Error("the file exceeded the limit but was not reported as truncated")
	}
	if got.Content == "" {
		t.Error("no content was returned for a readable text file")
	}
}

// TestBrowseTreeSkipsUnreadableEntries pins that one directory the server cannot read does not fail
// the whole listing. A checkout can hold a directory with restrictive modes committed to it, and a
// browse that returned an error for the whole project would make one such directory hide every
// playbook beside it.
func TestBrowseTreeSkipsUnreadableEntries(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can read a directory with no permissions")
	}
	s, checkout := seedCheckout(t, "proj_1", map[string]string{
		"site.yml":          "---\n",
		"locked/inside.yml": "---\n",
	})
	locked := filepath.Join(checkout, "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o750) })

	entries, err := s.Tree("proj_1")
	if err != nil {
		t.Fatalf("Tree() error = %v, want an unreadable directory to be skipped", err)
	}
	var paths []string
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	if diff := cmp.Diff([]string{"site.yml"}, paths, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("Tree() mismatch (-want +got):\n%s", diff)
	}
}

// TestCheckoutRootReportsAnUnresolvablePath pins that a checkout the server cannot resolve produces
// a plain failure rather than being mistaken for a project that was never synced. The two answers
// mean different things to an operator: one says sync it, the other says something is wrong on disk.
func TestCheckoutRootReportsAnUnresolvablePath(t *testing.T) {
	t.Parallel()
	s, checkout := seedCheckout(t, "proj_1", map[string]string{"site.yml": "---\n"})
	loop := filepath.Join(filepath.Dir(checkout), "proj_loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := s.checkoutRoot("proj_loop")
	if err == nil {
		t.Fatal("checkoutRoot() on a symlink loop = nil error, want a failure")
	}
	if errors.Is(err, ErrNoCheckout) {
		t.Error("a broken checkout was reported as never synced, which tells an operator to sync it")
	}
	if !strings.Contains(err.Error(), "resolve checkout") {
		t.Errorf("checkoutRoot() error = %v, want the resolve failure named", err)
	}
}
