package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/dcadolph/switchtender/internal/run"
)

func TestImportHandler(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Target     string
		Body       string
		WantStatus int
	}{
		{ // Test 0: An empty AWX export previews as an empty plan.
			Target: "/v1/import/awx", Body: "{}", WantStatus: http.StatusOK,
		},
		{ // Test 1: An unknown format is rejected.
			Target: "/v1/import/cobol", Body: "{}", WantStatus: http.StatusBadRequest,
		},
		{ // Test 2: Applying with no stores enabled is a conflict, not a crash.
			Target: "/v1/import/awx?apply=true", Body: "{}", WantStatus: http.StatusConflict,
		},
		{ // Test 3: Malformed export is rejected.
			Target: "/v1/import/awx", Body: "{not json", WantStatus: http.StatusBadRequest,
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop()).Handler()
			req := httptest.NewRequest(http.MethodPost, test.Target, strings.NewReader(test.Body))
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != test.WantStatus {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, test.WantStatus, rec.Body.String())
			}
		})
	}
}
