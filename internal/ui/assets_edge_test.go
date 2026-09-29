package ui

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"
)

// testAssetTree is a small asset subtree with the one js/ part the assembly requires, so a test can
// add whatever else it needs beside it.
func testAssetTree(extra fstest.MapFS) fstest.MapFS {
	tree := fstest.MapFS{"js/01-boot.js": {Data: []byte("const BOOT = 1;\n")}}
	for name, file := range extra {
		tree[name] = file
	}
	return tree
}

// TestAssetConditionalRequests pins how a browser revalidates a cached asset.
//
// The bundle is the largest thing the interface serves and its bytes never change for a given
// build, so every page load past the first has to come back as a not modified. Getting this wrong
// is not a correctness fault, it is the difference between a page that opens instantly and one that
// re-downloads the whole application each time. The other direction matters more: a new build must
// never be answered from a stale cache, which is what makes the tag content derived.
func TestAssetConditionalRequests(t *testing.T) {
	t.Parallel()
	body := bytes.Repeat([]byte("a { color: red; }\n"), 200)
	h := newAssetHandler(testAssetTree(fstest.MapFS{"app.css": {Data: body}}))
	etag := h.assets["app.css"].etag

	tests := []struct {
		Name        string
		IfNoneMatch string
		WantStatus  int
		WantBody    bool
	}{{ // Test 0: No validator at all is a plain download.
		Name: "no validator", WantStatus: http.StatusOK, WantBody: true,
	}, { // Test 1: The current tag is a not modified.
		Name: "current tag", IfNoneMatch: etag, WantStatus: http.StatusNotModified,
	}, { // Test 2: A tag from an earlier build is a fresh download.
		Name: "stale tag", IfNoneMatch: `"0000000000000000"`, WantStatus: http.StatusOK,
		WantBody: true,
	}, { // Test 3: A list of tags containing the current one is a not modified.
		Name: "tag among several", IfNoneMatch: `"deadbeef", ` + etag, WantStatus: http.StatusNotModified,
	}, { // Test 4: A weak validator carrying the tag is honored.
		Name: "weak validator", IfNoneMatch: "W/" + etag, WantStatus: http.StatusNotModified,
	}, { // Test 5: An empty header is ignored rather than treated as a match.
		Name: "empty validator", IfNoneMatch: "", WantStatus: http.StatusOK, WantBody: true,
	}, { // Test 6: A wildcard is not honored, so the asset is sent again.
		Name: "wildcard", IfNoneMatch: "*", WantStatus: http.StatusOK, WantBody: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/app.css", nil)
			if test.IfNoneMatch != "" {
				req.Header.Set("If-None-Match", test.IfNoneMatch)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != test.WantStatus {
				t.Fatalf("%s: status = %d, want %d", test.Name, rec.Code, test.WantStatus)
			}
			if got := rec.Header().Get("ETag"); got != etag {
				t.Errorf("%s: ETag = %q, want %q on every answer", test.Name, got, etag)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
				t.Errorf("%s: Cache-Control = %q, want no-cache so the browser revalidates",
					test.Name, got)
			}
			if test.WantBody && rec.Body.Len() == 0 {
				t.Errorf("%s: the asset was not sent", test.Name)
			}
			if !test.WantBody && rec.Body.Len() != 0 {
				t.Errorf("%s: a not modified answer carried %d bytes", test.Name, rec.Body.Len())
			}
		})
	}
}

// TestAssetETagsAreContentDerived proves two builds with different bytes get different tags and the
// same bytes get the same tag. Cache-Control is no-cache, so the tag is the only thing deciding
// whether an upgraded build reaches the browser at all.
func TestAssetETagsAreContentDerived(t *testing.T) {
	t.Parallel()
	first := newAssetHandler(testAssetTree(fstest.MapFS{"app.css": {Data: []byte("a{}\n")}}))
	same := newAssetHandler(testAssetTree(fstest.MapFS{"app.css": {Data: []byte("a{}\n")}}))
	changed := newAssetHandler(testAssetTree(fstest.MapFS{"app.css": {Data: []byte("a{ }\n")}}))

	if diff := cmp.Diff(first.assets["app.css"].etag, same.assets["app.css"].etag); diff != "" {
		t.Errorf("identical bytes produced different tags (-first +same):\n%s", diff)
	}
	if first.assets["app.css"].etag == changed.assets["app.css"].etag {
		t.Error("changed bytes kept the same tag, so an upgraded build is served from a stale cache")
	}
	// A changed script part must change the assembled bundle's tag too.
	other := newAssetHandler(fstest.MapFS{"js/01-boot.js": {Data: []byte("const BOOT = 2;\n")}})
	if first.assets["app.js"].etag == other.assets["app.js"].etag {
		t.Error("a changed script part left the bundle tag unchanged")
	}
}

// TestAssetContentTypes pins the type sent with each kind of file the interface ships.
//
// The mime package consults the operating system's tables at runtime, so a minimal container image
// answers with nothing for a font. A stylesheet or a script served as octet-stream is ignored by the
// browser, which is a blank page rather than an error anybody sees in a log.
func TestAssetContentTypes(t *testing.T) {
	t.Parallel()
	h := newAssetHandler(testAssetTree(fstest.MapFS{
		"app.css":          {Data: []byte("a{}\n")},
		"favicon.png":      {Data: []byte("\x89PNG\r\n")},
		"fonts/mono.woff2": {Data: []byte("wOF2")},
		"logo.svg":         {Data: []byte("<svg/>")},
		"notes.unknownext": {Data: []byte("x")},
		"noextension":      {Data: []byte("x")},
	}))

	tests := []struct {
		Name     string
		Path     string
		WantType string
	}{
		{Name: "stylesheet", Path: "/app.css", WantType: "text/css"},        // Test 0.
		{Name: "assembled script", Path: "/app.js", WantType: "javascript"}, // Test 1.
		{Name: "font", Path: "/fonts/mono.woff2", WantType: "font/woff2"},   // Test 2.
		{Name: "image", Path: "/favicon.png", WantType: "image/png"},        // Test 3.
		{Name: "unknown extension", Path: "/notes.unknownext", // Test 4.
			WantType: "application/octet-stream"},
		{Name: "no extension", Path: "/noextension", // Test 5.
			WantType: "application/octet-stream"},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, test.Path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200", test.Name, rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); !strings.Contains(got, test.WantType) {
				t.Errorf("%s: Content-Type = %q, want it to contain %q", test.Name, got,
					test.WantType)
			}
			if got := rec.Header().Values("Vary"); len(got) == 0 {
				t.Errorf("%s: no Vary header, so a shared cache may serve a gzip body to a client "+
					"that cannot read one", test.Name)
			}
		})
	}
}

// TestAssetGzipIsSkippedWhenItDoesNotHelp proves compression is not applied to a file it cannot
// shrink, and that such a file is still served correctly to a client asking for gzip. Sending a
// gzip body that is larger than the file wastes time at both ends, and sending a Content-Encoding
// header without a gzip body is a broken response.
func TestAssetGzipIsSkippedWhenItDoesNotHelp(t *testing.T) {
	t.Parallel()
	incompressible := make([]byte, 4096)
	if _, err := rand.Read(incompressible); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	small := []byte("a{color:red}\n")
	h := newAssetHandler(testAssetTree(fstest.MapFS{
		"noise.bin": {Data: incompressible},
		"tiny.css":  {Data: small},
	}))

	tests := []struct {
		Name string
		Path string
		Want []byte
	}{
		{Name: "incompressible file", Path: "/noise.bin", Want: incompressible}, // Test 0.
		{Name: "file too small to be worth it", Path: "/tiny.css", Want: small}, // Test 1.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, test.Path, nil)
			req.Header.Set("Accept-Encoding", "gzip, deflate, br")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if got := rec.Header().Get("Content-Encoding"); got != "" {
				t.Errorf("%s: Content-Encoding = %q, want none", test.Name, got)
			}
			if !bytes.Equal(rec.Body.Bytes(), test.Want) {
				t.Errorf("%s: the served bytes are not the file", test.Name)
			}
		})
	}
}

// TestGzipBodyBoundaries pins the size and benefit rules directly, at the edges where they change.
func TestGzipBodyBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		Body         []byte
		WantCompress bool
	}{
		{Name: "empty", Body: nil},                                           // Test 0.
		{Name: "one byte", Body: []byte("x")},                                // Test 1.
		{Name: "one below the floor", Body: bytes.Repeat([]byte("a"), 1023)}, // Test 2.
		{Name: "exactly at the floor", Body: bytes.Repeat([]byte("a"), 1024), // Test 3.
			WantCompress: true},
		{Name: "well above the floor", Body: bytes.Repeat([]byte("ab"), 5000), // Test 4.
			WantCompress: true},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := gzipBody(test.Body)
			if test.WantCompress && got == nil {
				t.Errorf("%s: no gzip body, want one", test.Name)
			}
			if !test.WantCompress && got != nil {
				t.Errorf("%s: a gzip body was produced for %d bytes, want none", test.Name,
					len(test.Body))
			}
			if got != nil && len(got) >= len(test.Body) {
				t.Errorf("%s: the gzip body is %d bytes against %d raw, so it should not have been "+
					"used", test.Name, len(got), len(test.Body))
			}
		})
	}
}

// TestAssetPathsAreLookedUpNotResolved proves a request cannot name anything outside the prepared
// set. Assets are held in a map keyed by the embedded name, so there is no file system walk to
// escape, and this is what pins that: a traversal is a plain miss rather than a read.
func TestAssetPathsAreLookedUpNotResolved(t *testing.T) {
	t.Parallel()
	h := newAssetHandler(testAssetTree(fstest.MapFS{"app.css": {Data: []byte("a{}\n")}}))

	tests := []struct {
		Name       string
		Path       string
		WantStatus int
	}{{ // Test 0: A real asset is served.
		Name: "real asset", Path: "/app.css", WantStatus: http.StatusOK,
	}, { // Test 1: A parent traversal names nothing.
		Name: "parent traversal", Path: "/../../etc/passwd", WantStatus: http.StatusNotFound,
	}, { // Test 2: An encoded traversal names nothing either.
		Name: "encoded traversal", Path: "/..%2f..%2fetc%2fpasswd", WantStatus: http.StatusNotFound,
	}, { // Test 3: An absolute path names nothing.
		Name: "absolute path", Path: "//etc/passwd", WantStatus: http.StatusNotFound,
	}, { // Test 4: A directory is not an asset.
		Name: "a directory", Path: "/js/", WantStatus: http.StatusNotFound,
	}, { // Test 5: The empty path is not an asset.
		Name: "empty path", Path: "/", WantStatus: http.StatusNotFound,
	}, { // Test 6: A name differing only in case is not the asset.
		Name: "wrong case", Path: "/APP.CSS", WantStatus: http.StatusNotFound,
	}, { // Test 7: A name with a trailing dot is not the asset.
		Name: "trailing dot", Path: "/app.css.", WantStatus: http.StatusNotFound,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, test.Path, nil))
			if rec.Code != test.WantStatus {
				t.Errorf("%s: GET %s status = %d, want %d", test.Name, test.Path, rec.Code,
					test.WantStatus)
			}
		})
	}
}

// refusingFS refuses to open any file, standing in for an embedded tree that cannot be read.
type refusingFS struct{ fs.FS }

// Open refuses every file so preparing the assets fails.
func (r refusingFS) Open(name string) (fs.File, error) {
	if name == "." {
		return r.FS.Open(name)
	}
	return nil, fs.ErrPermission
}

// TestNewAssetHandlerPanicsOnABrokenTree proves a build that cannot produce a serving set stops at
// startup rather than running with an interface that is quietly missing its script.
//
// Both faults here are build time errors: the tree is embedded, so a file that will not read or a
// missing set of script parts means the binary was assembled wrong. Failing loudly at construction
// is the only point where anybody sees it, since the alternative is a server that starts, answers
// health checks, and serves an application shell with no application in it.
func TestNewAssetHandlerPanicsOnABrokenTree(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Tree        fs.FS
		WantMessage string
	}{{ // Test 0: No script parts at all means there is no application to serve.
		Name:        "no script parts",
		Tree:        fstest.MapFS{"app.css": {Data: []byte("a{}\n")}},
		WantMessage: "no js/ source parts",
	}, { // Test 1: A file that cannot be read means the build is broken.
		Name:        "unreadable file",
		Tree:        refusingFS{fstest.MapFS{"app.css": {Data: []byte("a{}\n")}}},
		WantMessage: "prepare assets",
	}, { // Test 2: An empty tree has no script parts either.
		Name:        "empty tree",
		Tree:        fstest.MapFS{},
		WantMessage: "no js/ source parts",
	}, { // Test 3: A non-script file under js/ is not a script part.
		Name:        "only a note under js",
		Tree:        fstest.MapFS{"js/notes.md": {Data: []byte("# notes\n")}},
		WantMessage: "no js/ source parts",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("%s: a broken asset tree was accepted", test.Name)
				}
				msg, ok := r.(string)
				if !ok {
					t.Fatalf("%s: panicked with %T, want a message", test.Name, r)
				}
				if !strings.Contains(msg, test.WantMessage) {
					t.Errorf("%s: panic = %q, want it to mention %q", test.Name, msg,
						test.WantMessage)
				}
			}()
			newAssetHandler(test.Tree)
		})
	}
}

// TestAppJSKeepsPartOrder proves the assembled script concatenates its parts in name order.
//
// The numeric prefixes on the source files are load-bearing: constants have to initialize before the
// code below them runs. A map iteration order is random, so an assembly that walked the map directly
// would produce a working bundle most of the time and a broken one on some restarts, which is the
// worst possible failure to reproduce.
func TestAppJSKeepsPartOrder(t *testing.T) {
	t.Parallel()
	h := newAssetHandler(fstest.MapFS{
		"js/30-third.js":  {Data: []byte("third\n")},
		"js/10-first.js":  {Data: []byte("first\n")},
		"js/20-second.js": {Data: []byte("second\n")},
	})
	want := "first\nsecond\nthird\n"
	if diff := cmp.Diff(want, string(h.assets["app.js"].body)); diff != "" {
		t.Errorf("assembled script mismatch (-want +got):\n%s", diff)
	}
	// Nothing under js/ survives as its own entry.
	for name := range h.assets {
		if strings.HasPrefix(name, "js/") {
			t.Errorf("%q is still served on its own, but parts ship only inside app.js", name)
		}
	}
}

// TestAppJSSeparatesPartsWithoutTerminators proves every part boundary carries a newline, whichever
// side is missing one. A part saved without a trailing newline used to run its last statement into
// the next part's first, which is a syntax error for most pairs and a quietly different program for
// the rest, visible only in the served bundle.
func TestAppJSSeparatesPartsWithoutTerminators(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Tree     fstest.MapFS
		WantBody string
	}{{ // Test 0: Neither part ends with a newline.
		Name: "neither terminated",
		Tree: fstest.MapFS{
			"js/01.js": {Data: []byte("const A = 1;")},
			"js/02.js": {Data: []byte("const B = 2;")},
		},
		WantBody: "const A = 1;\nconst B = 2;\n",
	}, { // Test 1: Both parts already end with a newline and none is added.
		Name: "both terminated",
		Tree: fstest.MapFS{
			"js/01.js": {Data: []byte("const A = 1;\n")},
			"js/02.js": {Data: []byte("const B = 2;\n")},
		},
		WantBody: "const A = 1;\nconst B = 2;\n",
	}, { // Test 2: A single part is still terminated.
		Name:     "one part",
		Tree:     fstest.MapFS{"js/01.js": {Data: []byte("const A = 1;")}},
		WantBody: "const A = 1;\n",
	}, { // Test 3: An empty part does not swallow the boundary of the next one.
		Name: "an empty part",
		Tree: fstest.MapFS{
			"js/01.js": {Data: []byte("")},
			"js/02.js": {Data: []byte("const B = 2;")},
		},
		WantBody: "const B = 2;\n",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			h := newAssetHandler(test.Tree)
			if diff := cmp.Diff(test.WantBody, string(h.assets["app.js"].body)); diff != "" {
				t.Errorf("%s: assembled script mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}
