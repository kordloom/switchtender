package ui_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/ui"
)

// TestInAppDocsCarryHeadingAnchors pins that the docs rendered inside the product jump the way the
// published ones do.
//
// The marketing site renders the same markdown with heading ids; the in-app renderer did not. Every
// in-page anchor was therefore inert: a visible "download" link in the desktop guide moved nothing
// but the address bar, and every cross-guide link landed at the top of the right page rather than
// the section it named. The product shipped the broken copy of its own documentation.
func TestInAppDocsCarryHeadingAnchors(t *testing.T) {
	t.Parallel()
	// A real markdown tree, since the point is what goldmark emits for it.
	docs := fstest.MapFS{
		"guide.md": &fstest.MapFile{Data: []byte(
			"# Guide\n\nSee [download](#build-the-app-yourself) below.\n\n" +
				"## Build the app yourself\n\nSteps here.\n")},
	}
	handler := ui.New(zap.NewNop(), docs, false, 50000, false, false, false, "").Handler()

	req := httptest.NewRequest(http.MethodGet, "/ui/docs/guide", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/docs/guide status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if !regexp.MustCompile(`<h[1-6][^>]*\sid="`).MatchString(body) {
		t.Fatal("no heading carries an id, so every in-page anchor in the docs is inert")
	}

	// Every in-page link the page renders must have a target on the same page. A link to #download
	// with no element of that id is the exact failure this pins.
	anchors := regexp.MustCompile(`href="#([^"]+)"`).FindAllStringSubmatch(body, -1)
	for _, m := range anchors {
		if !strings.Contains(body, `id="`+m[1]+`"`) {
			t.Errorf("the page links to #%s but nothing on it carries that id, so the link is inert",
				m[1])
		}
	}
}
