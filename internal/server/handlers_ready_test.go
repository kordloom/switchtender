package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/run"
)

// unreachableStore is a run store whose status query always fails, standing in for a database that
// is down or still starting.
type unreachableStore struct {
	run.Store
}

// RunStatusCounts always fails.
func (unreachableStore) RunStatusCounts(context.Context) (map[run.Status]int, error) {
	return nil, errors.New("connection refused")
}

// healthSubmitter is a submitter that reports a fixed write health.
type healthSubmitter struct {
	fakeSubmitter
	// health is what Health returns.
	health dispatch.Health
}

// Health returns the fixed health.
func (h *healthSubmitter) Health() dispatch.Health { return h.health }

// TestReadyHandler proves readiness reflects the store and this process's writes to it, not just
// the process: a reachable store is 200 ready, an unreachable one is 503 not ready, and a store that
// answers while this process recently failed to write to it, or holds run ends it has not yet taken,
// is 200 ready and degraded with the reasons. Degraded stays in rotation so the UI that explains it
// stays up.
func TestReadyHandler(t *testing.T) {
	t.Parallel()
	failed := time.Date(2026, 10, 19, 23, 50, 0, 0, time.UTC)
	tests := []struct {
		Store      run.Store
		Submitter  Submitter
		WantStatus int
		WantBody   readyAnswer
	}{{ // Test 0: A reachable store and a submitter that reports nothing.
		Store: run.NewMemStore(), Submitter: &fakeSubmitter{},
		WantStatus: http.StatusOK, WantBody: readyAnswer{Ready: true},
	}, { // Test 1: An unreachable store.
		Store: unreachableStore{Store: run.NewMemStore()}, Submitter: &fakeSubmitter{},
		WantStatus: http.StatusServiceUnavailable,
		WantBody:   readyAnswer{Reason: "the run store is not reachable"},
	}, { // Test 2: Healthy writes.
		Store: run.NewMemStore(), Submitter: &healthSubmitter{},
		WantStatus: http.StatusOK, WantBody: readyAnswer{Ready: true},
	}, { // Test 3: A recent write failure and an end owed to the store.
		Store: run.NewMemStore(), Submitter: &healthSubmitter{health: dispatch.Health{
			Degraded: true, Reasons: []string{"disk full", "1 end owed"}, LastWriteFailure: failed,
			OwedEnds: 1,
		}},
		WantStatus: http.StatusOK, WantBody: readyAnswer{
			Ready: true, Degraded: true, Reasons: []string{"disk full", "1 end owed"},
			LastWriteFailure: &failed, OwedEnds: 1,
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			readyHandler(test.Store, test.Submitter).
				ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if rec.Code != test.WantStatus {
				t.Errorf("status = %d, want %d", rec.Code, test.WantStatus)
			}
			var got readyAnswer
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode %s: %v", rec.Body.String(), err)
			}
			if diff := cmp.Diff(test.WantBody, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("body mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
