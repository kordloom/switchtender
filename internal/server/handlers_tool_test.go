package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/run"
)

func TestCreateRunToolValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Body             string
		WantStatus       int
		WantBodyContains string
	}{
		{ // Test 0: A bash run with a command is accepted.
			Body: `{"tool":"bash","command":"echo hi"}`, WantStatus: http.StatusAccepted,
		},
		{ // Test 1: A bash run without a command is rejected.
			Body: `{"tool":"bash"}`, WantStatus: http.StatusBadRequest,
			WantBodyContains: "command is required",
		},
		{ // Test 2: An unknown tool is rejected.
			Body: `{"tool":"cobol","command":"x"}`, WantStatus: http.StatusBadRequest,
			WantBodyContains: "tool must be",
		},
		{ // Test 3: Ansible without a playbook is rejected.
			Body: `{"inventory":"hosts"}`, WantStatus: http.StatusBadRequest,
			WantBodyContains: "playbook is required",
		},
		{ // Test 4: Ansible with a playbook is accepted.
			Body: `{"playbook":"site.yml"}`, WantStatus: http.StatusAccepted,
		},
		{ // Test 5: A dry-run bash run is accepted.
			Body: `{"tool":"bash","command":"echo hi","dry_run":true}`, WantStatus: http.StatusAccepted,
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			sub := &fakeSubmitter{run: &run.Run{ID: "run_1", Status: run.StatusPending}}
			handler := New(run.NewMemStore(), sub, zap.NewNop()).Handler()
			req := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(test.Body))
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != test.WantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, test.WantStatus, rec.Body.String())
			}
			if test.WantBodyContains != "" && !strings.Contains(rec.Body.String(), test.WantBodyContains) {
				t.Errorf("body %q does not contain %q", rec.Body.String(), test.WantBodyContains)
			}
		})
	}
}
