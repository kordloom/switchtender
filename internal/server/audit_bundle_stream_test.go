package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
)

// noMaterializeStore refuses Chain, the whole-history read, while everything else works. It stands in
// for a long-lived install whose chain is too large to assemble per request: a handler that still
// works against it provably streams.
type noMaterializeStore struct {
	audit.Store
}

// Chain refuses the whole-history read.
func (s *noMaterializeStore) Chain(context.Context) ([]*audit.Entry, error) {
	return nil, errors.New("test: the handler materialized the whole chain")
}

// TestBundleHandlerStreamsTheChain proves the bundle export never loads the whole history to serve a
// request. The window used to be applied after materializing every entry, so the 250k cap defeated its
// own purpose: a limit=1000 request still assembled years of trail in memory, on an endpoint the site
// tells strangers to curl.
func TestBundleHandlerStreamsTheChain(t *testing.T) {
	inner := audit.NewMemStore()
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		e := &audit.Entry{ID: audit.NewID(), At: time.Now(), Actor: "alice", Method: "POST", Path: "/v1/runs"}
		if err := inner.Append(ctx, e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	h := auditBundleHandler(&noMaterializeStore{Store: inner}, &id, "v-test", zap.NewNop())

	for _, limit := range []string{"", "?limit=5"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/bundle"+limit, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("limit %q: status = %d, want 200; a non-200 here means the handler still "+
				"materializes the chain (body %s)", limit, rec.Code, rec.Body.String())
		}
	}
}
