package ui

import (
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"
)

// unreadableFS lists its files but refuses to open one of them, standing in for a tree entry the
// sidebar can see and the reader cannot.
type unreadableFS struct {
	// FS is the tree being wrapped.
	fs.FS
	// bad is the name that refuses to open.
	bad string
}

// Open refuses the one named file and passes everything else through.
func (u unreadableFS) Open(name string) (fs.File, error) {
	if name == u.bad {
		return nil, fs.ErrPermission
	}
	return u.FS.Open(name)
}

// testUI returns a UI wired to a documentation tree, so the unexported handlers can be driven
// directly.
func testUI(t *testing.T, docs fs.FS) *UI {
	t.Helper()
	return New(zap.NewNop(), docs, false, 0, false, false, false, "")
}

// getDoc asks for one documentation page by slug and returns the recorder.
func getDoc(t *testing.T, u *UI, slug string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/ui/docs/"+slug, nil)
	req.SetPathValue("page", slug)
	rec := httptest.NewRecorder()
	u.docsPage(rec, req)
	return rec
}

// cacheSize returns how many pages the documentation cache holds.
func cacheSize(u *UI) int {
	n := 0
	u.docCache.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// TestDocsCacheOnlyEverHoldsRealPages proves the cache is bounded by the embedded tree rather than
// by what a caller asks for.
//
// The cache is keyed by the slug from the request path, which anybody can choose. If a miss were
// stored, a few thousand requests for names that do not exist would grow the map for the life of
// the process, on a page that needs no authentication to be interesting. The handler must fill the
// cache only from pages it actually found.
func TestDocsCacheOnlyEverHoldsRealPages(t *testing.T) {
	t.Parallel()
	u := testUI(t, fstest.MapFS{
		"README.md": {Data: []byte("# Overview\n")},
		"guide.md":  {Data: []byte("# Guide\n")},
	})

	if rec := getDoc(t, u, "guide"); rec.Code != http.StatusOK {
		t.Fatalf("a real page = %d, want 200", rec.Code)
	}
	if got := cacheSize(u); got != 1 {
		t.Fatalf("after one real page the cache holds %d entries, want 1", got)
	}

	for i := range 2000 {
		slug := fmt.Sprintf("no-such-page-%d", i)
		if rec := getDoc(t, u, slug); rec.Code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404", slug, rec.Code)
		}
	}
	if got := cacheSize(u); got != 1 {
		t.Errorf("the cache grew to %d entries on requests for pages that do not exist, so a "+
			"caller chooses how much memory this holds", got)
	}
}

// TestDocsCacheFailsClosedOnAWrongEntry proves the handler refuses rather than serving whatever it
// finds when the cache holds something that is not a page. The cache is a sync.Map, so its values
// are untyped and the assertion that recovers a page is the only thing standing between a stray
// entry and a panic in the request path.
func TestDocsCacheFailsClosedOnAWrongEntry(t *testing.T) {
	t.Parallel()
	u := testUI(t, fstest.MapFS{"guide.md": {Data: []byte("# Guide\n")}})
	u.docCache.Store("guide", "this is not a page")

	rec := getDoc(t, u, "guide")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if strings.Contains(rec.Body.String(), "this is not a page") {
		t.Error("the handler served the stray cache entry back to the reader")
	}
}

// TestDocsSidebarSurvivesAnUnreadablePage proves one page that cannot be read does not take the
// whole sidebar down with it. The tree is embedded, so this is a build fault rather than a runtime
// one, and the reader must still get the pages that are fine.
func TestDocsSidebarSurvivesAnUnreadablePage(t *testing.T) {
	t.Parallel()
	u := testUI(t, unreadableFS{FS: fstest.MapFS{
		"README.md": {Data: []byte("# Overview\n")},
		"guide.md":  {Data: []byte("# The Guide\n")},
		"broken.md": {Data: []byte("# Broken\n")},
	}, bad: "broken.md"})

	links := u.docList("README")
	if len(links) != 3 {
		t.Fatalf("the sidebar has %d entries, want all three pages listed", len(links))
	}
	titles := map[string]string{}
	for _, l := range links {
		titles[l.Slug] = l.Title
	}
	// The unreadable page falls back to its slug rather than disappearing or breaking the list.
	if diff := cmp.Diff("broken", titles["broken"]); diff != "" {
		t.Errorf("unreadable page title mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff("The Guide", titles["guide"]); diff != "" {
		t.Errorf("readable page title mismatch (-want +got):\n%s", diff)
	}
	// The page itself is a not found rather than a server error.
	if rec := getDoc(t, u, "broken"); rec.Code != http.StatusNotFound {
		t.Errorf("an unreadable page = %d, want 404", rec.Code)
	}
}

// globFailFS serves its files but refuses to enumerate them, standing in for a tree the sidebar
// cannot list.
type globFailFS struct{ fs.FS }

// Glob refuses, so the sidebar has nothing to build from.
func (globFailFS) Glob(string) ([]string, error) { return nil, fs.ErrInvalid }

// TestDocsPageRendersWithoutASidebar proves a page still opens when the tree cannot be enumerated.
// The sidebar is navigation around the page, not the page, so losing it must cost the reader the
// list of other guides rather than the guide they asked for.
func TestDocsPageRendersWithoutASidebar(t *testing.T) {
	t.Parallel()
	u := testUI(t, globFailFS{fstest.MapFS{
		"README.md": {Data: []byte("# Overview\n")},
		"guide.md":  {Data: []byte("# The Guide\n\nBody text here.\n")},
	}})

	if got := u.docList("guide"); got != nil {
		t.Errorf("docList() = %v, want nil when the tree cannot be listed", got)
	}
	rec := getDoc(t, u, "guide")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the page itself is readable", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Body text here.") {
		t.Error("the page content is missing, so the reader lost the guide and not just the list")
	}
}

// TestDocsSidebarIsSortedByTitle proves the sidebar is ordered by what the reader sees, ignoring
// case, rather than by file name. A reader looks for a guide by its name, so the order has to follow
// the titles even when a file is named nothing like its heading.
func TestDocsSidebarIsSortedByTitle(t *testing.T) {
	t.Parallel()
	u := testUI(t, fstest.MapFS{
		"zeta.md":   {Data: []byte("# Alpha\n")},
		"alpha.md":  {Data: []byte("# zeta\n")},
		"middle.md": {Data: []byte("# Middle\n")},
	})
	links := u.docList("alpha")

	got := make([]string, 0, len(links))
	for _, l := range links {
		got = append(got, l.Title)
	}
	if diff := cmp.Diff([]string{"Alpha", "Middle", "zeta"}, got); diff != "" {
		t.Errorf("sidebar order mismatch (-want +got):\n%s", diff)
	}
	// Exactly one entry is marked as the page being read.
	active := 0
	for _, l := range links {
		if l.Active {
			active++
			if l.Slug != "alpha" {
				t.Errorf("the active entry is %q, want alpha", l.Slug)
			}
		}
	}
	if active != 1 {
		t.Errorf("%d sidebar entries are marked active, want exactly 1", active)
	}
}

// TestDocTitle pins how a page's name is found, since that name is what the sidebar sorts by and
// what the browser tab shows.
func TestDocTitle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		Data      string
		Slug      string
		WantTitle string
	}{
		{Name: "first line heading", Data: "# Overview\n\nBody.\n", Slug: "readme", // Test 0.
			WantTitle: "Overview"},
		{Name: "heading further down", Data: "<!-- note -->\n\n# Later\n", Slug: "s", // Test 1.
			WantTitle: "Later"},
		{Name: "no heading at all", Data: "just prose\n", Slug: "prose", // Test 2.
			WantTitle: "prose"},
		{Name: "empty file", Data: "", Slug: "empty", WantTitle: "empty"}, // Test 3.
		{Name: "sub heading only", Data: "## Second level\n", Slug: "sub", // Test 4.
			WantTitle: "sub"},
		{Name: "hash with no space", Data: "#NotAHeading\n", Slug: "hash", // Test 5.
			WantTitle: "hash"},
		{Name: "trailing whitespace", Data: "#   Padded   \n", Slug: "p", // Test 6.
			WantTitle: "Padded"},
		{Name: "windows line endings", Data: "# Windows\r\nBody\r\n", Slug: "w", // Test 7.
			WantTitle: "Windows"},
		{Name: "non latin heading", Data: "# 運用ガイド\n", Slug: "jp", // Test 8.
			WantTitle: "運用ガイド"},
		{Name: "the first heading wins", Data: "# First\n\n# Second\n", Slug: "f", // Test 9.
			WantTitle: "First"},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantTitle, docTitle([]byte(test.Data), test.Slug)); diff != "" {
				t.Errorf("%s: title mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestValidSlug pins exactly which names reach the documentation tree.
//
// The slug is joined to a file name, so this is the boundary that decides what a request can name
// on disk. It has to refuse anything with a separator, a dot, or a byte outside the small alphabet,
// and it has to keep accepting the two names the app itself uses.
func TestValidSlug(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		Slug      string
		WantValid bool
	}{
		{Name: "empty means the overview", Slug: "", WantValid: true},        // Test 0.
		{Name: "the overview by name", Slug: "README", WantValid: true},      // Test 1.
		{Name: "lower case word", Slug: "concepts", WantValid: true},         // Test 2.
		{Name: "dashes and digits", Slug: "tool-ansible-2", WantValid: true}, // Test 3.
		{Name: "a single letter", Slug: "a", WantValid: true},                // Test 4.
		{Name: "leading dash", Slug: "-guide", WantValid: true},              // Test 5.
		{Name: "upper case", Slug: "Concepts"},                               // Test 6.
		{Name: "underscore", Slug: "my_guide"},                               // Test 7.
		{Name: "a dot", Slug: "guide.md"},                                    // Test 8.
		{Name: "a slash", Slug: "sub/guide"},                                 // Test 9.
		{Name: "parent traversal", Slug: "../secrets"},                       // Test 10.
		{Name: "absolute path", Slug: "/etc/passwd"},                         // Test 11.
		{Name: "a space", Slug: "my guide"},                                  // Test 12.
		{Name: "a null byte", Slug: "guide\x00"},                             // Test 13.
		{Name: "a newline", Slug: "guide\n"},                                 // Test 14.
		{Name: "non latin", Slug: "ガイド"},                                     // Test 15.
		{Name: "a backslash", Slug: `sub\guide`},                             // Test 16.
		{Name: "url encoded traversal", Slug: "%2e%2e%2f"},                   // Test 17.
		{Name: "very long but in the alphabet", Slug: strings.Repeat("a", 5000), // Test 18.
			WantValid: true},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := validSlug(test.Slug); got != test.WantValid {
				t.Errorf("%s: validSlug(%q) = %v, want %v", test.Name, test.Slug, got,
					test.WantValid)
			}
		})
	}
}

// TestRewriteDocLinks pins which links are turned into app routes and, more importantly, which are
// left alone. The rewrite runs over rendered HTML, so a pattern that reached too far would rewrite
// an outbound link on a page the reader trusts.
func TestRewriteDocLinks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		HTML     string
		WantHTML string
	}{{ // Test 0: A sibling page becomes its app route.
		Name: "sibling page", HTML: `<a href="concepts.md">c</a>`,
		WantHTML: `<a href="/ui/docs/concepts">c</a>`,
	}, { // Test 1: The overview lives at the docs root rather than under a slug.
		Name: "the overview", HTML: `<a href="README.md">home</a>`,
		WantHTML: `<a href="/ui/docs">home</a>`,
	}, { // Test 2: An anchor is carried through onto the app route.
		Name: "with an anchor", HTML: `<a href="guide.md#step-two">s</a>`,
		WantHTML: `<a href="/ui/docs/guide#step-two">s</a>`,
	}, { // Test 3: An anchor on the overview keeps the root.
		Name: "overview anchor", HTML: `<a href="README.md#top">t</a>`,
		WantHTML: `<a href="/ui/docs#top">t</a>`,
	}, { // Test 4: A path is reduced to the page name, since the app serves a flat set of slugs.
		Name: "nested path", HTML: `<a href="sub/dir/page.md">p</a>`,
		WantHTML: `<a href="/ui/docs/page">p</a>`,
	}, { // Test 5: A relative prefix is dropped the same way.
		Name: "dot slash prefix", HTML: `<a href="./page.md">p</a>`,
		WantHTML: `<a href="/ui/docs/page">p</a>`,
	}, { // Test 6: Several links in one document are all rewritten.
		Name: "two links", HTML: `<a href="a.md">a</a> and <a href="b.md#x">b</a>`,
		WantHTML: `<a href="/ui/docs/a">a</a> and <a href="/ui/docs/b#x">b</a>`,
	}, { // Test 7: An outbound link to somebody else's markdown is left alone.
		Name: "external markdown", HTML: `<a href="https://example.com/readme.md">x</a>`,
		WantHTML: `<a href="https://example.com/readme.md">x</a>`,
	}, { // Test 8: An app link already in its final form is untouched.
		Name: "already an app route", HTML: `<a href="/ui/runs">runs</a>`,
		WantHTML: `<a href="/ui/runs">runs</a>`,
	}, { // Test 9: A file whose name merely starts with md is not a markdown link.
		Name: "not a markdown extension", HTML: `<a href="notes.markdown">n</a>`,
		WantHTML: `<a href="notes.markdown">n</a>`,
	}, { // Test 10: An image source is not a link and is left alone.
		Name: "image source", HTML: `<img src="diagram.md">`, WantHTML: `<img src="diagram.md">`,
	}, { // Test 11: A document with no links is returned unchanged.
		Name: "no links", HTML: `<p>Nothing to rewrite.</p>`,
		WantHTML: `<p>Nothing to rewrite.</p>`,
	}, { // Test 12: An empty document is returned unchanged.
		Name: "empty", HTML: "", WantHTML: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantHTML, rewriteDocLinks(test.HTML)); diff != "" {
				t.Errorf("%s: rewrite mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestDocHref pins the two shapes a documentation URL takes, since the sidebar builds every link
// with it and the overview is the one page that does not live under a slug.
func TestDocHref(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Slug     string
		WantHref string
	}{
		{Name: "the overview", Slug: "README", WantHref: "/ui/docs"},           // Test 0.
		{Name: "a page", Slug: "concepts", WantHref: "/ui/docs/concepts"},      // Test 1.
		{Name: "a dashed page", Slug: "tool-go", WantHref: "/ui/docs/tool-go"}, // Test 2.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantHref, docHref(test.Slug)); diff != "" {
				t.Errorf("%s: href mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestRenderFailsClosedWhenATemplateBreaks proves a page that cannot render answers with a server
// error rather than a blank two hundred. The interface has no other error path: every page goes
// through this one function, so a render fault that returned a success status would show the reader
// an empty page and tell the monitoring nothing happened.
func TestRenderFailsClosedWhenATemplateBreaks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Template   string
		WantStatus int
	}{{ // Test 0: A template that faults before writing anything.
		Name: "fault before any output", Template: `{{.Name.Sub}}`,
		WantStatus: http.StatusInternalServerError,
	}, { // Test 1: A template that ranges over something that is not a list.
		Name: "range over a string", Template: `{{range .Name}}x{{end}}`,
		WantStatus: http.StatusInternalServerError,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			u := &UI{
				tmpl: template.Must(template.New("page.html").Parse(test.Template)),
				log:  zap.NewNop(),
			}
			rec := httptest.NewRecorder()
			u.render(rec, "page.html", map[string]any{"Name": "text"})
			if rec.Code != test.WantStatus {
				t.Errorf("%s: status = %d, want %d", test.Name, rec.Code, test.WantStatus)
			}
		})
	}
}

// TestRenderOfAnUnknownTemplateIsAnError proves asking for a page that is not in the parsed set is a
// server error rather than an empty success. A route registered against a template name that no
// longer exists is the shape this catches.
func TestRenderOfAnUnknownTemplateIsAnError(t *testing.T) {
	t.Parallel()
	u := testUI(t, nil)
	rec := httptest.NewRecorder()
	u.render(rec, "no-such-template.html", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if !strings.Contains(rec.Body.String(), "internal error") {
		t.Errorf("body = %q, want the internal error message", rec.Body.String())
	}
}

// TestNewWithoutALoggerStillRenders proves the constructor supplies its own logger when given none,
// including on the path that logs a render failure. A nil logger there would turn a page fault into
// a panic that takes the whole server process down.
func TestNewWithoutALoggerStillRenders(t *testing.T) {
	t.Parallel()
	u := New(nil, nil, false, 0, false, false, false, "")
	if u.log == nil {
		t.Fatal("New left the logger nil, so any render failure panics")
	}
	rec := httptest.NewRecorder()
	u.render(rec, "no-such-template.html", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

// TestRenderReportsAPartialPageAsAFailure asserts a page whose template faults after it has already
// written output is reported as a failure rather than as a success.
//
// Once bytes are on the wire the status line is already sent, so the error message is appended to a
// half-drawn page and the reader gets a two hundred. Every page in the interface is a single
// template with no partial-write protection in front of it, so any render fault past the first
// action lands this way: the reader sees a broken page, and nothing counting statuses ever notices.
func TestRenderReportsAPartialPageAsAFailure(t *testing.T) {
	t.Parallel()
	u := &UI{
		tmpl: template.Must(template.New("page.html").Parse(`<html><body>{{.Name.Sub}}`)),
		log:  zap.NewNop(),
	}
	rec := httptest.NewRecorder()
	u.render(rec, "page.html", map[string]any{"Name": "text"})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d: a half-drawn page is not a success", rec.Code,
			http.StatusInternalServerError)
	}
	if strings.Contains(rec.Body.String(), "internal error") &&
		strings.Contains(rec.Body.String(), "<html>") {
		t.Error("the error message was appended to a partly rendered page")
	}
}
