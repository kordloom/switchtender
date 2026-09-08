package relay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// leakRuns is how many runs a worker transport is driven through in a leak check. It is far above
// any concurrency a worker has, so state kept per run rather than per in-flight run is obvious.
const leakRuns = 200

// held returns how many batches and leases the transport is still holding.
func (t *httpTransport) held() (batches, leases int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.batches), len(t.leases)
}

// leaseServer answers a claim with a fresh run and a fresh capability, and every other call with
// 204, standing in for a control node that is accepting everything a worker reports.
func leaseServer(t *testing.T) *httptest.Server {
	t.Helper()
	var claimed int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/relay/v1/claim" {
			claimed++
			w.Header().Set(leaseHeader, fmt.Sprintf("lease-%d", claimed))
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"run_%d","playbook":"site.yml","status":"pending"}`, claimed)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSettledRunsLeaveNoTransportState pins that a worker transport gives back everything it held
// for a run once that run has finished and its report has landed.
//
// A worker process is long lived and claims run after run for weeks. It holds a buffered log batch
// and a per-claim capability per run it has claimed, keyed by run id, and both are secret-bearing:
// the capability is exactly the proof another process would need to report over this run. Anything
// kept per run rather than per in-flight run grows for the life of the worker and keeps that
// material in memory long after the last report it could authorize.
func TestSettledRunsLeaveNoTransportState(t *testing.T) {
	t.Parallel()
	srv := leaseServer(t)
	transport, ok := NewHTTPTransport(srv.URL, "worker-token", srv.Client()).(*httpTransport)
	if !ok {
		t.Fatal("NewHTTPTransport() did not return an *httpTransport")
	}

	ctx := context.Background()
	for i := 0; i < leakRuns; i++ {
		leased, err := transport.Claim(ctx, "w1", nil)
		if err != nil {
			t.Fatalf("Claim() error = %v", err)
		}
		// Half the runs produce output and half produce none, because the two release their state
		// down different paths: one through the batch's final flush, one through the terminal save
		// alone.
		if i%2 == 0 {
			if err := transport.AppendLog(ctx, leased.ID, []byte("some output\n")); err != nil {
				t.Fatalf("AppendLog() error = %v", err)
			}
		}
		leased.Status = run.StatusSucceeded
		if err := transport.Save(ctx, leased); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	batches, leases := transport.held()
	if batches != 0 || leases != 0 {
		t.Errorf("after %d settled runs the transport holds %d batches and %d leases, want 0 and 0",
			leakRuns, batches, leases)
	}
}
