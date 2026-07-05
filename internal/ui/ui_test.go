package ui_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/ui"
)

func TestUIRoutes(t *testing.T) {
	t.Parallel()
	handler := ui.New(zap.NewNop()).Handler()

	tests := []struct {
		Name         string
		Path         string
		WantStatus   int
		WantContains string
	}{
		{ // Test 0: History page renders.
			Name: "index", Path: "/ui/", WantStatus: http.StatusOK, WantContains: `data-page="index"`,
		},
		{ // Test 1: Detail page carries the run id.
			Name: "detail", Path: "/ui/runs/run_1", WantStatus: http.StatusOK,
			WantContains: `data-run-id="run_1"`,
		},
		{ // Test 2: Stylesheet is served.
			Name: "css", Path: "/ui/assets/app.css", WantStatus: http.StatusOK, WantContains: "--bg",
		},
		{ // Test 3: Script is served.
			Name: "js", Path: "/ui/assets/app.js", WantStatus: http.StatusOK, WantContains: "buildModel",
		},
		{ // Test 4: Fleet page renders.
			Name: "fleet", Path: "/ui/fleet", WantStatus: http.StatusOK, WantContains: `data-page="fleet"`,
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
