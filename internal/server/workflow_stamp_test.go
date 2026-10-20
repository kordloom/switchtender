package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// TestTheWorkflowPageCarriesTheRelease pins that the server hands its release to the pages, so the
// workflow editor drops a draft an older release saved.
func TestTheWorkflowPageCarriesTheRelease(t *testing.T) {
	t.Parallel()
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithProducerIdentity(nil, "v-test")).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/workflows", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/workflows = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `data-draft-stamp="v-test"`) {
		t.Error("the workflow page does not carry the server's release as its draft stamp")
	}
}
