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

// TestReservedIdempotencyKeysAreRefusedEverywhere checks that no client-facing submission accepts a
// key in the namespace the product derives its own keys from.
//
// Derived keys look like st:trigger:<id>:<bucket>. They are how a redelivered webhook, a rerun, and
// a shard retry avoid firing twice. If a caller can mint one, they can plant a run under the key a
// later launch will compute: the webhook then resolves to the planted run, answers the git host 202,
// and never deploys. A deployment that silently does not happen is worse than one that fails.
//
// The run endpoint validated this from the start. The pipeline endpoint took the header verbatim,
// which is the whole bug: one of two doors was locked.
func TestReservedIdempotencyKeysAreRefusedEverywhere(t *testing.T) {
	t.Parallel()
	reserved := []string{
		"st:trigger:tg_deploy:178563219",
		"st:rerun:run_abc:178563219",
		"st:retry-shards:run_abc:178563219",
		"st:anything",
	}
	endpoints := []struct {
		Name string
		Path string
		Body string
	}{
		{"runs", "/v1/runs", `{"playbook":"site.yml","inventory":"inv"}`},
		{"pipelines", "/v1/pipelines", `{"name":"deploy","inventory":"inv",` +
			`"steps":[{"name":"one","playbook":"site.yml"}]}`},
	}
	for _, ep := range endpoints {
		for testNum, key := range reserved {
			t.Run(fmt.Sprintf("%s %d", ep.Name, testNum), func(t *testing.T) {
				t.Parallel()
				sub := &fakeSubmitter{run: &run.Run{ID: "run_1", Status: run.StatusPending}}
				handler := New(run.NewMemStore(), sub, zap.NewNop()).Handler()
				req := httptest.NewRequest(http.MethodPost, ep.Path, strings.NewReader(ep.Body))
				req.Header.Set(idempotencyKeyHeader, key)
				rec := httptest.NewRecorder()

				handler.ServeHTTP(rec, req)

				if rec.Code < 400 {
					t.Errorf("POST %s accepted the reserved key %q with %d, so a caller can plant "+
						"a run under a key a later webhook or rerun will derive, and that launch "+
						"resolves to the plant instead of executing", ep.Path, key, rec.Code)
				}
			})
		}
	}

	// An ordinary client key still works on both, so the guard did not close the feature.
	for _, ep := range endpoints {
		sub := &fakeSubmitter{run: &run.Run{ID: "run_1", Status: run.StatusPending}}
		handler := New(run.NewMemStore(), sub, zap.NewNop()).Handler()
		req := httptest.NewRequest(http.MethodPost, ep.Path, strings.NewReader(ep.Body))
		req.Header.Set(idempotencyKeyHeader, "deploy-2026-08-02-attempt-1")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code >= 400 {
			t.Errorf("POST %s refused an ordinary idempotency key: %d %s",
				ep.Path, rec.Code, rec.Body.String())
		}
	}
}
