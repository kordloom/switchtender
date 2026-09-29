package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// TestCreateRunAppliesTheHostLimit asserts that a limit in the run body reaches the submitted run.
//
// A field the request struct does not declare is dropped by the decoder rather than refused, so
// asking to touch one canary host and being answered 202 proved nothing: the run went to every host
// in the inventory and the reply looked identical. The assertion is therefore on the submitted run's
// limit, not on the status code.
func TestCreateRunAppliesTheHostLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Body is the JSON posted to the runs endpoint.
		Body string
		// WantLimit is the host pattern the submitted run must carry.
		WantLimit string
	}{
		{ // Test 0: A limit on an ad-hoc Ansible run narrows the submitted run.
			Body: `{"playbook":"site.yml","inventory":"hosts","limit":"web01"}`, WantLimit: "web01",
		},
		{ // Test 1: A limit on a non-Ansible run is carried the same way.
			Body: `{"tool":"bash","command":"echo hi","limit":"canary"}`, WantLimit: "canary",
		},
		{ // Test 2: A pattern naming several hosts survives intact.
			Body:      `{"playbook":"site.yml","limit":"web01,web02:&staging"}`,
			WantLimit: "web01,web02:&staging",
		},
		{ // Test 3: No limit leaves the run unnarrowed.
			Body: `{"playbook":"site.yml","inventory":"hosts"}`, WantLimit: "",
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

			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusAccepted, rec.Body)
			}
			if sub.gotRun == nil {
				t.Fatal("no run was submitted")
			}
			if diff := cmp.Diff(test.WantLimit, sub.gotRun.Limit); diff != "" {
				t.Errorf("submitted run limit mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
