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
	handler := ui.New(zap.NewNop(), nil, false, 50000, false, false, false, "").Handler()

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
	handler := ui.New(zap.NewNop(), docs, false, 0, false, false, false, "").Handler()

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

// TestAppJSAssembledFromParts pins the app.js assembly: the served script is exactly the
// embedded js/ source parts concatenated in name order, and the parts themselves are not
// served. The order is part of the program, so a part that goes missing or serves alone is a
// broken build, not a smaller download.
func TestAppJSAssembledFromParts(t *testing.T) {
	t.Parallel()
	handler := ui.New(zap.NewNop(), nil, false, 50000, false, false, false, "").Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/assets/app.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("app.js = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if len(body) < 100000 {
		t.Fatalf("app.js is %d bytes, too small to be the assembled application", len(body))
	}
	// Spot functions from the first, a middle, and the last source part must all be present,
	// which fails if any part is dropped from the assembly.
	for _, marker := range []string{"function syncBrandLogos", "const auditCollections", "function buildModel"} {
		if !strings.Contains(body, marker) {
			t.Errorf("assembled app.js is missing %q, so a source part was dropped", marker)
		}
	}
	// A source part must not be reachable on its own.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/assets/js/01-boot.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("js/01-boot.js = %d, want 404: parts ship only inside app.js", rec.Code)
	}
}

// TestLoginBrandedSSO proves the sign-in page renders the OIDC button with the configured provider's
// label, and falls back to a generic single sign-on button for an unbranded issuer.
func TestLoginBrandedSSO(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Brand        string
		WantContains string
	}{
		{Brand: "google", WantContains: "Sign in with Google"},
		{Brand: "microsoft", WantContains: "Sign in with Microsoft"},
		{Brand: "github", WantContains: "Sign in with GitHub"},
		{Brand: "gitlab", WantContains: "Sign in with GitLab"},
		{Brand: "okta", WantContains: "Sign in with Okta"},
		{Brand: "", WantContains: "Sign in with SSO"},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			handler := ui.New(zap.NewNop(), nil, false, 0, true, false, false, test.Brand).Handler()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/login", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("login status = %d, want 200", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, test.WantContains) {
				t.Errorf("login for brand %q missing %q", test.Brand, test.WantContains)
			}
			if !strings.Contains(body, "/auth/oidc/login") {
				t.Errorf("login for brand %q has no OIDC link", test.Brand)
			}
		})
	}
}

// TestLoginDemoShowcase proves a read-only demo with no identity provider still advertises SSO with a
// disabled button, so a visitor sees the capability exists, and never a live sign-in link.
func TestLoginDemoShowcase(t *testing.T) {
	t.Parallel()
	handler := ui.New(zap.NewNop(), nil, true, 0, false, false, false, "").Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/login", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "sso-demo-note") {
		t.Error("the demo login does not advertise SSO")
	}
	if !strings.Contains(body, "disabled") {
		t.Error("the demo SSO button is not disabled")
	}
	if strings.Contains(body, "/auth/oidc/login") || strings.Contains(body, "/auth/saml/login") {
		t.Error("the demo login exposes a live SSO link with no provider configured")
	}
}
