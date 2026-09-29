package ui_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/ui"
)

func TestUIRoutes(t *testing.T) {
	t.Parallel()
	handler := ui.New(zap.NewNop(), nil, false, 50000, false, false).Handler()

	tests := []struct {
		Name         string
		Path         string
		WantStatus   int
		WantContains string
	}{
		{ // Test 0: Overview home renders.
			Name: "overview", Path: "/ui/", WantStatus: http.StatusOK,
			WantContains: `data-page="overview"`,
		},
		{ // Test 1: Runs page renders.
			Name: "runs", Path: "/ui/runs", WantStatus: http.StatusOK, WantContains: `data-page="runs"`,
		},
		{ // Test 2: Detail page carries the run id.
			Name: "detail", Path: "/ui/runs/run_1", WantStatus: http.StatusOK,
			WantContains: `data-run-id="run_1"`,
		},
		{ // Test 2b: Detail page carries the configured matrix cap.
			Name: "detail matrix cap", Path: "/ui/runs/run_1", WantStatus: http.StatusOK,
			WantContains: `data-matrix-cap="50000"`,
		},
		{ // Test 3: Stylesheet is served.
			Name: "css", Path: "/ui/assets/app.css", WantStatus: http.StatusOK, WantContains: "--bg",
		},
		{ // Test 4: Script is served.
			Name: "js", Path: "/ui/assets/app.js", WantStatus: http.StatusOK, WantContains: "buildModel",
		},
		{ // Test 5: Fleet page renders.
			Name: "fleet", Path: "/ui/fleet", WantStatus: http.StatusOK, WantContains: `data-page="fleet"`,
		},
		{ // Test 6: Schedules page renders.
			Name: "schedules", Path: "/ui/schedules", WantStatus: http.StatusOK,
			WantContains: `data-page="schedules"`,
		},
		{ // Test 6b: Workflow editor page renders.
			Name: "workflows", Path: "/ui/workflows", WantStatus: http.StatusOK,
			WantContains: `data-page="workflows"`,
		},
		{ // Test 7: Migrate page renders.
			Name: "migrate", Path: "/ui/migrate", WantStatus: http.StatusOK,
			WantContains: `data-page="migrate"`,
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, test.Path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.WantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, test.WantStatus)
			}
			if !strings.Contains(rec.Body.String(), test.WantContains) {
				t.Errorf("body does not contain %q", test.WantContains)
			}
		})
	}
}

func TestUIDocs(t *testing.T) {
	t.Parallel()
	docs := fstest.MapFS{
		"README.md":   {Data: []byte("# Overview\n\nWelcome to the docs.\n")},
		"concepts.md": {Data: []byte("# Concepts\n\n| A | B |\n|---|---|\n| 1 | 2 |\n")},
	}
	handler := ui.New(zap.NewNop(), docs, false, 0, false, false).Handler()

	tests := []struct {
		Name         string
		Path         string
		WantStatus   int
		WantContains string
	}{
		{ // Test 0: The docs root renders the overview and the sidebar.
			Name: "root", Path: "/ui/docs", WantStatus: http.StatusOK,
			WantContains: `data-page="docs"`,
		},
		{ // Test 1: A page renders its markdown, including GFM tables.
			Name: "page", Path: "/ui/docs/concepts", WantStatus: http.StatusOK,
			WantContains: "<table>",
		},
		{ // Test 2: A missing page is a 404, not a server error.
			Name: "missing", Path: "/ui/docs/nope", WantStatus: http.StatusNotFound,
		},
		{ // Test 3: A traversal attempt is rejected.
			Name: "traversal", Path: "/ui/docs/..%2f..%2fetc", WantStatus: http.StatusNotFound,
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, test.Path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.WantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, test.WantStatus)
			}
			if test.WantContains != "" && !strings.Contains(rec.Body.String(), test.WantContains) {
				t.Errorf("body does not contain %q", test.WantContains)
			}
		})
	}
}
