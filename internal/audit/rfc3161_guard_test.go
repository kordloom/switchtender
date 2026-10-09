package audit

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestTimestampDefaultClientIsGuarded pins the client a timestamp request uses when the caller
// supplies none: it refuses a blocked address at the dial and still reaches an authority on this
// host, which the administrator rule allows. The authority's address is operator configuration a
// server follows on its own network, and a default client follows it anywhere.
//
// The authority stands behind the unspecified address, which the guard refuses and which a plain
// dial turns into this host, so a client that reaches it is a client with no guard.
func TestTimestampDefaultClientIsGuarded(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	tsa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(tsa.Close)
	link := strings.Repeat("ab", 32)

	tests := []struct {
		// Name labels the case in failure output.
		Name string
		// URL is the timestamp authority the request is sent to.
		URL string
		// WantReached is whether the request must arrive at the authority.
		WantReached bool
	}{{ // Test 0: A blocked address is refused before anything is sent.
		Name: "blocked address", URL: strings.Replace(tsa.URL, "127.0.0.1", "0.0.0.0", 1) + "/tsr",
		WantReached: false,
	}, { // Test 1: An authority on this host is reached, and its empty reply is then refused.
		Name: "loopback authority", URL: tsa.URL + "/tsr", WantReached: true,
	}}
	// The cases share the arrival counter, so they run in order.
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			before := hits.Load()
			if _, err := Timestamp(context.Background(), nil, test.URL, link); err == nil {
				t.Fatalf("%s: Timestamp() error = nil, want the request refused", test.Name)
			}
			reached := hits.Load()-before > 0
			if reached != test.WantReached {
				t.Errorf("%s: reached = %v, want %v", test.Name, reached, test.WantReached)
			}
		})
	}
}
