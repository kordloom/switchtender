package ui

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"
)

// TestAssembleAppJSSeparatesParts proves the assembled script keeps a line boundary between source
// parts even when a part's last line has no terminator.
//
// The parts were concatenated as raw bytes, so a file saved without a trailing newline ran its last
// statement into the next file's first one. That is a syntax error for most pairs and a quietly
// different program for the rest, it can only be seen in the served bundle rather than in any
// source file, and the node test loader joins the same parts with its own newline, so the suite
// would keep passing while the shipped script failed to parse.
func TestAssembleAppJSSeparatesParts(t *testing.T) {
	t.Parallel()
	h := newAssetHandler(fstest.MapFS{
		"js/01-first.js":  {Data: []byte("const FIRST = 1;")},
		"js/02-second.js": {Data: []byte("function second() { return FIRST; }\n")},
	})
	want := "const FIRST = 1;\nfunction second() { return FIRST; }\n"
	if diff := cmp.Diff(want, string(h.assets["app.js"].body)); diff != "" {
		t.Errorf("app.js mismatch (-want +got):\n%s", diff)
	}
}

// TestAssembleAppJSKeepsPartsUnservable proves nothing under js/ answers on its own URL, whatever
// its extension.
//
// Only the .js parts were removed from the served map, so any other file parked in that directory
// stayed reachable by name while never appearing in app.js. The parts ship inside app.js and
// nowhere else.
func TestAssembleAppJSKeepsPartsUnservable(t *testing.T) {
	t.Parallel()
	h := newAssetHandler(fstest.MapFS{
		"js/01-first.js": {Data: []byte("const FIRST = 1;\n")},
		"js/notes.md":    {Data: []byte("# internal notes\n")},
		"js/old.js.bak":  {Data: []byte("const SECRET = 1;\n")},
		"app.css":        {Data: []byte(":root{}\n")},
	})

	tests := []struct {
		Name       string
		Path       string
		WantStatus int
	}{{ // Test 0: The assembled script is served.
		Name: "app.js", Path: "/app.js", WantStatus: http.StatusOK,
	}, { // Test 1: A source part is not served on its own.
		Name: "part", Path: "/js/01-first.js", WantStatus: http.StatusNotFound,
	}, { // Test 2: A non-script file beside the parts is not served either.
		Name: "note", Path: "/js/notes.md", WantStatus: http.StatusNotFound,
	}, { // Test 3: Nor is a backup copy of a part.
		Name: "backup", Path: "/js/old.js.bak", WantStatus: http.StatusNotFound,
	}, { // Test 4: Assets outside js/ are unaffected.
		Name: "css", Path: "/app.css", WantStatus: http.StatusOK,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, test.Path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != test.WantStatus {
				t.Errorf("GET %s = %d, want %d", test.Path, rec.Code, test.WantStatus)
			}
		})
	}
}

// TestAssetGzipIsLazyAndCorrect proves gzip is not computed at construction but is served correctly
// on demand and reused. Compressing every asset at boot was tens of milliseconds on the startup
// path that a health probe or an API-only caller never needed, so it now happens on first request.
func TestAssetGzipIsLazyAndCorrect(t *testing.T) {
	t.Parallel()
	big := bytes.Repeat([]byte("alertable and compressible content;\n"), 200)
	h := newAssetHandler(fstest.MapFS{"app.css": {Data: big}, "js/01.js": {Data: []byte("var x=1;\n")}})

	// Nothing is compressed until a request asks for it.
	if h.assets["app.css"].gzipped != nil {
		t.Fatal("gzip body was computed at construction; it must be lazy")
	}

	// A gzip-accepting request gets gzip that decodes back to the exact body.
	req := httptest.NewRequest(http.MethodGet, "/app.css", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	round, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip: %v", err)
	}
	if !bytes.Equal(round, big) {
		t.Error("gzip body did not decode back to the original")
	}

	// The compression is computed once and cached on the shared entry.
	if h.assets["app.css"].gzipped == nil {
		t.Fatal("gzip body was not cached after first use")
	}

	// A client that does not accept gzip gets the raw body.
	plain := httptest.NewRequest(http.MethodGet, "/app.css", nil)
	prec := httptest.NewRecorder()
	h.ServeHTTP(prec, plain)
	if prec.Header().Get("Content-Encoding") != "" || !bytes.Equal(prec.Body.Bytes(), big) {
		t.Error("a non-gzip client should get the raw body")
	}
}

// TestAssetGzipConcurrent runs many gzip requests for one asset at once, so the race detector proves
// the lazy compute is safe under load.
func TestAssetGzipConcurrent(t *testing.T) {
	t.Parallel()
	h := newAssetHandler(fstest.MapFS{"app.css": {Data: bytes.Repeat([]byte("x=1;\n"), 500)}, "js/01.js": {Data: []byte("var x=1;\n")}})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/app.css", nil)
			req.Header.Set("Accept-Encoding", "gzip")
			h.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	wg.Wait()
}
