package ui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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
