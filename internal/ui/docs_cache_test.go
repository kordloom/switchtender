package ui_test

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/ui"
)

// countingFS wraps a documentation tree and counts how many times a file is opened, so a test can
// see whether a page is rendered again on a second request.
type countingFS struct {
	// FS is the tree being counted.
	fs.FS
	// mu guards opens.
	mu sync.Mutex
	// opens is how many files have been opened.
	opens int
}

// Open records the open and passes it to the wrapped tree.
func (c *countingFS) Open(name string) (fs.File, error) {
	c.mu.Lock()
	c.opens++
	c.mu.Unlock()
	return c.FS.Open(name)
}

// count returns how many files have been opened so far.
func (c *countingFS) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opens
}

// TestDocsPagesAreRenderedOnce proves a documentation page is built on its first request and reused
// after that, and that the page it serves is unchanged.
//
// Every request used to run the markdown through goldmark, run a regular expression over the whole
// rendered result to rewrite cross-page links, and then read every other page off the tree to
// rebuild the same sidebar. The tree is embedded, so none of that could produce a different answer
// the second time.
func TestDocsPagesAreRenderedOnce(t *testing.T) {
	t.Parallel()
	docs := &countingFS{FS: fstest.MapFS{
		"README.md":   {Data: []byte("# Overview\n\nSee [concepts](concepts.md).\n")},
		"concepts.md": {Data: []byte("# Concepts\n\n| A | B |\n|---|---|\n| 1 | 2 |\n")},
	}}
	handler := ui.New(zap.NewNop(), docs, false, 0, false, false, false).Handler()

	get := func(t *testing.T, path string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want %d", path, rec.Code, http.StatusOK)
		}
		return rec.Body.String()
	}

	tests := []struct {
		Name         string
		Path         string
		WantContains string
	}{{ // Test 0: The docs root, whose link to another page is rewritten to an app route.
		Name: "root", Path: "/ui/docs", WantContains: `href="/ui/docs/concepts"`,
	}, { // Test 1: A page whose markdown carries a GFM table.
		Name: "page", Path: "/ui/docs/concepts", WantContains: "<table>",
	}}
	// The cases share one counter, so they run in sequence: a parallel case's reads would land in
	// another's before-and-after difference.
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			first := get(t, test.Path)
			if !strings.Contains(first, test.WantContains) {
				t.Fatalf("body does not contain %q", test.WantContains)
			}
			opens := docs.count()
			second := get(t, test.Path)
			if second != first {
				t.Errorf("the second request rendered a different page")
			}
			if got := docs.count(); got != opens {
				t.Errorf("the second request read the tree %d more times, want 0", got-opens)
			}
		})
	}
}
