package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

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

// TestReadyHandler proves readiness reflects the store, not just the process: a reachable store is
// 200 ready, an unreachable one is 503 not ready, so a load balancer holds traffic off a starting or
// broken replica instead of routing to it.
func TestReadyHandler(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	readyHandler(run.NewMemStore()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("reachable store status = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	readyHandler(unreachableStore{Store: run.NewMemStore()}).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unreachable store status = %d, want 503", rec.Code)
	}
}
