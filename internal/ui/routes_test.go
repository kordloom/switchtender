package ui_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/ui"
)

// navLink matches the href of every in-app link the rendered pages carry, so a
// nav entry that points at a route nobody registered is caught here rather than
// by the first person who clicks it.
var navLink = regexp.MustCompile(`href="(/ui/[^"#?]*)"`)

// TestUnknownPathIsNotFound checks that a path under /ui/ that no handler claims
// is refused. The overview handler is registered on the subtree pattern "/ui/",
// which matches every unclaimed path beneath it, so without an explicit check a
// typo renders the overview page with a 200 and the reader is told nothing.
func TestUnknownPathIsNotFound(t *testing.T) {
	t.Parallel()
	handler := ui.New(zap.NewNop(), nil, false, 50000, false, false, false, "").Handler()

	tests := []struct {
		Name       string
		Path       string
		WantStatus int
	}{{ // Test 0: The overview home itself still renders.
		Name: "overview home", Path: "/ui/", WantStatus: http.StatusOK,
	}, { // Test 1: A near miss on a real route is refused, not silently rewritten.
		Name: "singular of a real route", Path: "/ui/workflow", WantStatus: http.StatusNotFound,
	}, { // Test 2: An invented path is refused.
		Name: "invented path", Path: "/ui/totally-fake-xyz", WantStatus: http.StatusNotFound,
	}, { // Test 3: A deep invented path is refused.
		Name: "deep invented path", Path: "/ui/one/two/three", WantStatus: http.StatusNotFound,
	}, { // Test 4: A trailing slash on a real route is refused rather than falling through.
		Name: "trailing slash on a real route", Path: "/ui/runs/", WantStatus: http.StatusNotFound,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, test.Path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.WantStatus {
				t.Errorf("GET %s status = %d, want %d", test.Path, rec.Code, test.WantStatus)
			}
		})
	}
}

// TestEveryNavLinkResolves walks every page the navigation offers and follows
// each in-app link it carries, so a route renamed on one side of the app and not
// the other fails the build instead of shipping.
func TestEveryNavLinkResolves(t *testing.T) {
	t.Parallel()
	handler := ui.New(zap.NewNop(), nil, false, 50000, false, false, false, "").Handler()

	pages := []string{
		"/ui/", "/ui/runs", "/ui/activity", "/ui/fleet", "/ui/drift", "/ui/tasks",
		"/ui/workers", "/ui/projects", "/ui/inventories", "/ui/sources", "/ui/templates",
		"/ui/workflows", "/ui/schedules", "/ui/migrate", "/ui/credentials", "/ui/users",
		"/ui/audit", "/ui/policies", "/ui/doctor", "/ui/login",
	}

	seen := map[string]bool{}
	for _, page := range pages {
		req := httptest.NewRequest(http.MethodGet, page, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("nav page %s status = %d, want %d", page, rec.Code, http.StatusOK)
			continue
		}
		for _, match := range navLink.FindAllStringSubmatch(rec.Body.String(), -1) {
			link := match[1]
			if seen[link] || strings.HasPrefix(link, "/ui/assets/") {
				continue
			}
			seen[link] = true
			linkReq := httptest.NewRequest(http.MethodGet, link, nil)
			linkRec := httptest.NewRecorder()
			handler.ServeHTTP(linkRec, linkReq)
			if linkRec.Code != http.StatusOK {
				t.Errorf("link %q on page %s status = %d, want %d",
					link, page, linkRec.Code, http.StatusOK)
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("no in-app links were found on any page; the extraction is wrong")
	}
	t.Logf("followed %d distinct in-app links across %d pages", len(seen), len(pages))
}
