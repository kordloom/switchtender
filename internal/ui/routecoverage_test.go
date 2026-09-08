package ui_test

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/ui"
)

// routePattern finds every route the handler registers, so this file works from the same list the
// server does rather than from a copy that drifts away from it.
var routePattern = regexp.MustCompile(`mux\.Handle(?:Func)?\("GET ([^"]+)"`)

// wildcardValues fill the path wildcards a route declares. A route is only exercised if something
// plausible is put in each segment.
var wildcardValues = map[string]string{
	"{id}":   "run_1",
	"{host}": "web1.example.com",
	"{page}": "guide",
}

// testDocs is a small documentation tree, so the routes that only exist when docs are wired are
// registered and reachable.
var testDocs = fstest.MapFS{
	"README.md": {Data: []byte("# Overview\n\nSee [the guide](guide.md).\n")},
	"guide.md":  {Data: []byte("# Guide\n\nSteps.\n")},
}

// registeredRoutes reads the handler's own registration list out of the source beside this test and
// returns each route as a concrete request path.
func registeredRoutes(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("ui.go")
	if err != nil {
		t.Fatalf("read ui.go: %v", err)
	}
	matches := routePattern.FindAllStringSubmatch(string(src), -1)
	if len(matches) < 20 {
		t.Fatalf("found %d registered routes in ui.go, which is too few: the extraction is wrong",
			len(matches))
	}
	seen := map[string]bool{}
	paths := make([]string, 0, len(matches))
	for _, m := range matches {
		path := m[1]
		if path == "/ui/assets/" {
			// The asset subtree is served by its own handler, so it is exercised by a real file.
			path = "/ui/assets/app.css"
		}
		for wildcard, value := range wildcardValues {
			path = strings.ReplaceAll(path, wildcard, value)
		}
		if strings.Contains(path, "{") {
			t.Fatalf("route %q has a wildcard with no test value; add one to wildcardValues", path)
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// TestEveryRegisteredRouteAnswers walks the handler's own route list and proves each one answers.
//
// The route table is written by hand and every page is a separate method, so a route can be
// registered against a template name that does not exist, or a template can be renamed on one side
// only. Nothing catches that at build time: the failure is a five hundred on a page nobody opened
// during review. Reading the list out of the source rather than restating it here means a route
// added tomorrow is covered tomorrow, without anyone remembering to add it.
func TestEveryRegisteredRouteAnswers(t *testing.T) {
	t.Parallel()
	handler := ui.New(zap.NewNop(), testDocs, false, 5000, true, true, true, "google").Handler()

	routes := registeredRoutes(t)
	for testNum, path := range routes {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want %d", path, rec.Code, http.StatusOK)
			}
			if rec.Body.Len() == 0 {
				t.Errorf("GET %s returned an empty body", path)
			}
			if strings.HasPrefix(path, "/ui/assets/") {
				return
			}
			if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
				t.Errorf("GET %s content type = %q, want html", path, got)
			}
			// A page that rendered only part way still returns two hundred, so the closing tag is
			// the cheap proof that the template ran to the end.
			if !strings.Contains(rec.Body.String(), "</html>") {
				t.Errorf("GET %s did not render a complete page", path)
			}
		})
	}
	t.Logf("exercised %d registered routes", len(routes))
}

// TestPagesRenderInEveryConfiguration renders every route under each combination of the switches New
// takes, because those switches decide what a page shows rather than only how it looks.
//
// A read-only demo must not draw a control that mutates, and an install with no AI provider must not
// draw an ask panel that fails on the first question. Each switch reaches the templates through a
// different key, so a page that ignores one renders something the operator was told it would not.
func TestPagesRenderInEveryConfiguration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		ReadOnly  bool
		MatrixCap int
		OIDC      bool
		SAML      bool
		AI        bool
		Brand     string
	}{
		{Name: "plain"},                          // Test 0.
		{Name: "read only demo", ReadOnly: true}, // Test 1.
		{Name: "everything on", MatrixCap: 1, OIDC: true, SAML: true, AI: true, // Test 2.
			Brand: "okta"},
		{Name: "negative matrix cap", MatrixCap: -1},    // Test 3.
		{Name: "saml only", SAML: true},                 // Test 4.
		{Name: "unbranded oidc", OIDC: true},            // Test 5.
		{Name: "unknown brand", OIDC: true, Brand: "a"}, // Test 6.
	}
	routes := registeredRoutes(t)
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			handler := ui.New(zap.NewNop(), testDocs, test.ReadOnly, test.MatrixCap, test.OIDC,
				test.SAML, test.AI, test.Brand).Handler()
			for _, path := range routes {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
				if rec.Code != http.StatusOK {
					t.Errorf("%s: GET %s status = %d, want %d", test.Name, path, rec.Code,
						http.StatusOK)
				}
			}
		})
	}
}

// TestOnlyGetIsAnswered proves the web interface answers reads and refuses everything else.
//
// Every route is registered as GET, so a write method must be refused by the router rather than
// reaching a handler that would render a page in response to a POST. A page that answers a POST
// with two hundred is a page a cross-site form submission can drive.
func TestOnlyGetIsAnswered(t *testing.T) {
	t.Parallel()
	handler := ui.New(zap.NewNop(), testDocs, false, 0, false, false, false, "").Handler()

	tests := []struct {
		Name       string
		Method     string
		Path       string
		WantStatus int
	}{{ // Test 0: A read of the overview is answered.
		Name: "get", Method: http.MethodGet, Path: "/ui/", WantStatus: http.StatusOK,
	}, { // Test 1: HEAD is a read and is answered, which is what a link checker sends.
		Name: "head", Method: http.MethodHead, Path: "/ui/", WantStatus: http.StatusOK,
	}, { // Test 2: A form post to a page is refused.
		Name: "post", Method: http.MethodPost, Path: "/ui/", WantStatus: http.StatusMethodNotAllowed,
	}, { // Test 3: A post to a named page is refused.
		Name: "post to users", Method: http.MethodPost, Path: "/ui/users",
		WantStatus: http.StatusMethodNotAllowed,
	}, { // Test 4: A put is refused.
		Name: "put", Method: http.MethodPut, Path: "/ui/credentials",
		WantStatus: http.StatusMethodNotAllowed,
	}, { // Test 5: A delete is refused.
		Name: "delete", Method: http.MethodDelete, Path: "/ui/policies",
		WantStatus: http.StatusMethodNotAllowed,
	}, { // Test 6: A patch is refused.
		Name: "patch", Method: http.MethodPatch, Path: "/ui/runs/run_1",
		WantStatus: http.StatusMethodNotAllowed,
	}, { // Test 7: A post to an asset is refused.
		Name: "post to asset", Method: http.MethodPost, Path: "/ui/assets/app.css",
		WantStatus: http.StatusMethodNotAllowed,
	}, { // Test 8: A post to a documentation page is refused.
		Name: "post to docs", Method: http.MethodPost, Path: "/ui/docs/guide",
		WantStatus: http.StatusMethodNotAllowed,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(test.Method, test.Path, nil))
			if rec.Code != test.WantStatus {
				t.Errorf("%s %s status = %d, want %d", test.Method, test.Path, rec.Code,
					test.WantStatus)
			}
		})
	}
}

// TestPathValuesAreEscapedIntoThePage proves a value taken from the URL is escaped before it is
// written into the page.
//
// The run id, the host name, and the docs slug all come from the request path and land inside HTML
// attributes. A run id is not a trusted string: anyone who can hand somebody a link controls it. If
// it were written raw, a crafted link would close the attribute and run script in the operator's
// session, against a control plane whose whole purpose is executing commands on a fleet.
func TestPathValuesAreEscapedIntoThePage(t *testing.T) {
	t.Parallel()
	handler := ui.New(zap.NewNop(), testDocs, false, 0, false, false, false, "").Handler()

	tests := []struct {
		Name    string
		Route   string
		Value   string
		WantOut string
	}{{ // Test 0: An attribute-closing payload in a run id.
		Name: "run id breaking out of an attribute", Route: "/ui/runs/",
		Value: `x" onmouseover="alert(1)`, WantOut: `x" onmouseover=`,
	}, { // Test 1: A tag-opening payload in a run id.
		Name: "run id opening a tag", Route: "/ui/runs/", Value: `<script>alert(1)</script>`,
		WantOut: "<script>alert(1)",
	}, { // Test 2: The same payload on the comparison page.
		Name: "compare page", Route: "/ui/runs/", Value: `x"><img src=x onerror=alert(1)>`,
		WantOut: `x"><img`,
	}, { // Test 3: A host name is equally untrusted.
		Name: "host name", Route: "/ui/hosts/", Value: `web1" onload="alert(1)`,
		WantOut: `web1" onload=`,
	}, { // Test 4: A single quoted payload, in case the attribute is quoted the other way.
		Name: "single quotes", Route: "/ui/hosts/", Value: `web1' onload='alert(1)`,
		WantOut: `web1' onload=`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			path := test.Route + url.PathEscape(test.Value)
			if test.Name == "compare page" {
				path += "/compare"
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want %d", path, rec.Code, http.StatusOK)
			}
			if strings.Contains(rec.Body.String(), test.WantOut) {
				t.Errorf("%s: the page contains %q unescaped, so a crafted link runs script in "+
					"the reader's session", test.Name, test.WantOut)
			}
		})
	}
}

// TestPathValuesAtTheirBoundaries proves the pages that take a value from the URL cope with the
// shapes a real request can carry: nothing, one character, a value in another script, and a value
// far longer than anything the product mints.
func TestPathValuesAtTheirBoundaries(t *testing.T) {
	t.Parallel()
	handler := ui.New(zap.NewNop(), testDocs, false, 0, false, false, false, "").Handler()

	tests := []struct {
		Name       string
		Path       string
		WantStatus int
	}{{ // Test 0: A run id of one character renders.
		Name: "single character run id", Path: "/ui/runs/a", WantStatus: http.StatusOK,
	}, { // Test 1: An empty run id matches no route and is refused.
		Name: "empty run id", Path: "/ui/runs/", WantStatus: http.StatusNotFound,
	}, { // Test 2: An empty host is refused rather than rendering a page about no host.
		Name: "empty host", Path: "/ui/hosts/", WantStatus: http.StatusNotFound,
	}, { // Test 3: A host in a non-Latin script renders.
		Name: "unicode host", Path: "/ui/hosts/" + url.PathEscape("ホスト-1"),
		WantStatus: http.StatusOK,
	}, { // Test 4: A very long run id renders rather than failing.
		Name: "very long run id", Path: "/ui/runs/" + strings.Repeat("r", 8000),
		WantStatus: http.StatusOK,
	}, { // Test 5: An empty run id on the comparison path is cleaned away by the router and the
		// reader is redirected rather than shown a page about no run.
		Name: "compare with no id", Path: "/ui/runs//compare",
		WantStatus: http.StatusTemporaryRedirect,
	}, { // Test 6: A comparison page for a real run id renders.
		Name: "compare with an id", Path: "/ui/runs/run_1/compare", WantStatus: http.StatusOK,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, test.Path, nil))
			if rec.Code != test.WantStatus {
				t.Errorf("%s: GET %s status = %d, want %d", test.Name, test.Path, rec.Code,
					test.WantStatus)
			}
		})
	}
}

// TestDocsRoutesAreAbsentWithoutATree proves a build with no documentation tree does not answer the
// documentation routes at all, rather than answering them with the overview page or a server error.
// The overview is registered on the subtree pattern, so anything unclaimed beneath it lands there.
func TestDocsRoutesAreAbsentWithoutATree(t *testing.T) {
	t.Parallel()
	handler := ui.New(zap.NewNop(), nil, false, 0, false, false, false, "").Handler()

	for testNum, path := range []string{"/ui/docs", "/ui/docs/guide", "/ui/docs/"} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusNotFound {
				t.Errorf("GET %s with no docs tree status = %d, want %d", path, rec.Code,
					http.StatusNotFound)
			}
		})
	}
}

// TestDocsSlugsAreBounded proves the documentation route serves only pages that exist and refuses
// every shape of name that is not a slug.
//
// The slug is joined to a file name and read off the tree, so it is the one place in the interface
// where a request names a file. It is also the cache key, so an accepted name that resolves to
// nothing would still be a name a caller chose.
func TestDocsSlugsAreBounded(t *testing.T) {
	t.Parallel()
	handler := ui.New(zap.NewNop(), testDocs, false, 0, false, false, false, "").Handler()

	tests := []struct {
		Name       string
		Path       string
		WantStatus int
	}{{ // Test 0: A page that exists is served.
		Name: "real page", Path: "/ui/docs/guide", WantStatus: http.StatusOK,
	}, { // Test 1: The root serves the overview.
		Name: "root", Path: "/ui/docs", WantStatus: http.StatusOK,
	}, { // Test 2: A slug that looks fine but names no page is a not found.
		Name: "missing page", Path: "/ui/docs/nothing-here", WantStatus: http.StatusNotFound,
	}, { // Test 3: A parent directory reference is refused.
		Name: "parent traversal", Path: "/ui/docs/..%2f..%2fetc%2fpasswd",
		WantStatus: http.StatusNotFound,
	}, { // Test 4: A slug carrying its own extension is refused.
		Name: "slug with an extension", Path: "/ui/docs/guide.md", WantStatus: http.StatusNotFound,
	}, { // Test 5: An absolute path is refused.
		Name: "absolute path", Path: "/ui/docs/%2fetc%2fpasswd", WantStatus: http.StatusNotFound,
	}, { // Test 6: An upper-case slug is refused rather than reaching the tree.
		Name: "upper case", Path: "/ui/docs/Guide", WantStatus: http.StatusNotFound,
	}, { // Test 7: An underscore is not part of a slug.
		Name: "underscore", Path: "/ui/docs/my_guide", WantStatus: http.StatusNotFound,
	}, { // Test 8: A non-Latin slug is refused.
		Name: "unicode", Path: "/ui/docs/" + url.PathEscape("ガイド"),
		WantStatus: http.StatusNotFound,
	}, { // Test 9: A null byte in the slug is refused.
		Name: "null byte", Path: "/ui/docs/guide%00.md", WantStatus: http.StatusNotFound,
	}, { // Test 10: A very long slug is refused by the tree rather than accepted.
		Name: "very long slug", Path: "/ui/docs/" + strings.Repeat("a", 4000),
		WantStatus: http.StatusNotFound,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, test.Path, nil))
			if rec.Code != test.WantStatus {
				t.Errorf("%s: GET %s status = %d, want %d", test.Name, test.Path, rec.Code,
					test.WantStatus)
			}
		})
	}
}

// TestShippedDocsAreSelfConsistent walks the documentation tree the binary actually ships and proves
// every page it lists is a page it will serve.
//
// The sidebar builds a link for every markdown file on the tree, and the page handler accepts only
// lowercase slugs. Nothing holds those two rules together, so a guide added under a name with a
// capital in it appears in the sidebar of every page and answers a not found when anybody clicks it.
func TestShippedDocsAreSelfConsistent(t *testing.T) {
	t.Parallel()
	tree := os.DirFS("../../docs")
	if _, err := fs.Stat(tree, "README.md"); err != nil {
		t.Skipf("the documentation tree is not beside this package: %v", err)
	}
	handler := ui.New(zap.NewNop(), tree, false, 0, false, false, false, "").Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/docs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/docs status = %d, want %d", rec.Code, http.StatusOK)
	}
	links := regexp.MustCompile(`href="(/ui/docs[^"#]*)"`).FindAllStringSubmatch(rec.Body.String(), -1)
	if len(links) < 5 {
		t.Fatalf("the docs sidebar carries %d links, too few to be the shipped tree", len(links))
	}
	seen := map[string]bool{}
	for _, match := range links {
		link := match[1]
		if seen[link] {
			continue
		}
		seen[link] = true
		linkRec := httptest.NewRecorder()
		handler.ServeHTTP(linkRec, httptest.NewRequest(http.MethodGet, link, nil))
		if linkRec.Code != http.StatusOK {
			t.Errorf("the docs sidebar links to %q, which answers %d: a page the reader is "+
				"offered has to open", link, linkRec.Code)
		}
	}
	t.Logf("followed %d documentation links", len(seen))
}
