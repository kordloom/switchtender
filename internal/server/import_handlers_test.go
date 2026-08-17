package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

func TestImportHandler(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Target     string
		Body       string
		WantStatus int
	}{
		{ // Test 0: An export holding nothing this importer reads is refused rather than previewed as
			// a plan of zeros, which used to read as a migration that had succeeded.
			Target: "/v1/import/awx", Body: "{}", WantStatus: http.StatusUnprocessableEntity,
		},
		{ // Test 1: An unknown format is rejected.
			Target: "/v1/import/cobol", Body: "{}", WantStatus: http.StatusBadRequest,
		},
		{ // Test 2: An apply of a document holding nothing is refused for the same reason, before it
			// ever reaches the question of which stores are enabled.
			Target: "/v1/import/awx?apply=true", Body: "{}", WantStatus: http.StatusUnprocessableEntity,
		},
		{ // Test 4: A real export with no stores enabled is a conflict, not a crash.
			Target: "/v1/import/semaphore?apply=true",
			Body: `{"projects":[{"name":"acme","templates":[{"name":"deploy",` +
				`"playbook":"site.yml"}]}]}`,
			WantStatus: http.StatusConflict,
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

// TestImportRundeckFormat proves the Rundeck importer is reachable over the API and that the target
// inventory named in the query reaches the imported templates.
func TestImportRundeckFormat(t *testing.T) {
	t.Parallel()
	export := `
- name: Nightly
  schedule:
    crontab: '0 0 2 * * ? *'
  options:
    - name: secret_token
      secure: true
  sequence:
    commands:
      - exec: nightly.sh
`
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop()).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/v1/import/rundeck?inventory=prod-hosts", strings.NewReader(export)))
	if rec.Code != http.StatusOK {
		t.Fatalf("import status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Nightly") {
		t.Errorf("preview does not name the imported job: %s", body)
	}
	// A secure option must be reported as refused, never quietly imported as a plaintext survey field.
	if !strings.Contains(body, "secret_token") {
		t.Errorf("preview does not report the refused secure option: %s", body)
	}

	// An unknown format is still refused, and the message names what is accepted.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/import/jenkins",
		strings.NewReader("{}")))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown format = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "rundeck") {
		t.Errorf("the error does not name rundeck as accepted: %s", rec.Body.String())
	}
}
