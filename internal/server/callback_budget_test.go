package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/roundhouse"
)

// budgetReplica starts a server on s with a callback budget of perMinute requests per address, the
// way a second process behind a load balancer shares the first one's database.
func budgetReplica(t *testing.T, s cbkStores, perMinute int) http.Handler {
	t.Helper()
	runner := roundhouse.RunnerFunc(func(context.Context, roundhouse.Spec,
		io.Writer) (roundhouse.Result, error) {
		return roundhouse.Result{}, nil
	})
	d := dispatch.New(s.runs, runner, zap.NewNop(), dispatch.WithInventories(s.inventories),
		dispatch.WithNoJanitor(), dispatch.WithQueues([]string{"cbk-unserved"}),
		dispatch.WithRunFilesRoot(t.TempDir()))
	t.Cleanup(d.Close)
	return New(s.runs, d, zap.NewNop(), WithTemplates(s.templates),
		WithInventories(s.inventories), WithAudit(&recordingAudits{}),
		WithCredentials(credential.NewMemStore(), credential.NewSealer("pass", "salt")),
		func(srv *Server) { srv.callbackResolver = fakeResolver{} },
		WithCallbackLimitMatcher(webLimit()), WithCallbackRateLimits(perMinute, 0)).Handler()
}

// TestCallbackRequestBudgetHoldsAcrossReplicas spends one address's callback budget on one replica
// and calls again through a second replica on the same database. docs/configuration.md promises an
// address past --callback-rate-limit callbacks in a minute is refused for the rest of the minute; a
// budget kept in each process gave the address the whole budget again on every replica.
func TestCallbackRequestBudgetHoldsAcrossReplicas(t *testing.T) {
	t.Parallel()
	const perMinute = 3
	stores := cbkMemoryStores()
	cbkSeed(t, stores)
	a, b := budgetReplica(t, stores, perMinute), budgetReplica(t, stores, perMinute)
	key := (&cbkReplica{handler: a}).mint(t)
	call := func(h http.Handler) *httptest.ResponseRecorder {
		// The right key from an address the inventory does not hold: admitted, then refused for
		// the address, so only the request budget is spent and never the wrong-key one.
		req := httptest.NewRequest(http.MethodPost, cbkNative, strings.NewReader(keyBody(key)))
		req.RemoteAddr = "10.99.0.1:40000"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for i := 0; i < perMinute; i++ {
		if rec := call(a); rec.Code != http.StatusBadRequest {
			t.Fatalf("callback %d on replica A = %d, want 400 for an unknown address: %s", i,
				rec.Code, rec.Body.String())
		}
	}
	if rec := call(b); rec.Code != http.StatusTooManyRequests {
		t.Errorf("replica B admitted a callback from an address whose budget replica A spent: "+
			"status %d, want 429: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}
