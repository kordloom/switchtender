package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRelayGateRoutesWorkersPastTheAPITokenGate pins the boundary that lets a mesh worker
// authenticate with its own token: /relay/ requests reach the relay handler untouched, and
// everything else does not.
//
// The gate sits OUTSIDE the API token check on purpose, mirroring webhook triggers carrying their
// own secret. That placement is also exactly why it needs its own test: a routing mistake here is
// not a 404, it is either a worker checked against the wrong credential store, refusing the whole
// fleet, or an API request slipped to the relay handler, answered by a surface whose
// authentication model was never meant for it. It sat at true-zero coverage while carrying that
// responsibility.
func TestRelayGateRoutesWorkersPastTheAPITokenGate(t *testing.T) {
	t.Parallel()
	mark := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Handled-By", name)
			w.WriteHeader(http.StatusOK)
		})
	}
	gate := relayGate(mark("relay"), mark("api"))

	tests := []struct {
		// Path is the request path as a worker or client would send it.
		Path string
		// Want is which handler must answer.
		Want string
	}{
		{"/relay/claim", "relay"},  // Test 0: The worker surface.
		{"/relay/", "relay"},       // Test 1: The bare prefix is still the worker surface.
		{"/v1/runs", "api"},        // Test 2: The API stays behind the token gate.
		{"/relayish", "api"},       // Test 3: A prefix look-alike is not the worker surface.
		{"/v1/relay/claim", "api"}, // Test 4: The segment elsewhere in the path is not it.
		{"/", "api"},               // Test 5: The UI root.
	}
	for _, test := range tests {
		r := httptest.NewRequest(http.MethodPost, test.Path, nil)
		rec := httptest.NewRecorder()
		gate.ServeHTTP(rec, r)
		if got := rec.Header().Get("X-Handled-By"); got != test.Want {
			t.Errorf("%s was answered by %q, want %q: a worker checked against API tokens locks "+
				"out the fleet, and an API request on the relay surface answers to the wrong "+
				"authentication model", test.Path, got, test.Want)
		}
	}
}
