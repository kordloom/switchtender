package forward

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestHTTPSinkDefaultClientGuardsTheAuditStream pins the client an HTTP sink gets when the server
// hands it none: it refuses a blocked address at the dial, stops at a redirect, and still reaches a
// collector on this host, which the administrator rule allows.
//
// The redirect case is the one that costs something. A plain client follows a 302 by turning the
// POST into a GET somewhere else, then reports the batch delivered on whatever that answers, so
// the cursor moves past events no collector ever stored.
//
// The blocked case stands the collector behind the unspecified address, which the guard refuses
// and which a plain dial turns into this host, so a client that reaches it is a client with no
// guard.
func TestHTTPSinkDefaultClientGuardsTheAuditStream(t *testing.T) {
	t.Parallel()
	var direct, landed atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hop":
			http.Redirect(w, r, srv.URL+"/landed", http.StatusFound)
		case "/landed":
			landed.Add(1)
		default:
			direct.Add(1)
		}
	}))
	t.Cleanup(srv.Close)
	refused := strings.Replace(srv.URL, "127.0.0.1", "0.0.0.0", 1)
	events := []Event{{ID: "aud_1", Seq: 1, Receipt: "1:aa"}}

	tests := []struct {
		// Name labels the case in failure output.
		Name string
		// URL is the collector address the sink posts to.
		URL string
		// Reached counts the deliveries that arrived where this case must or must not land.
		Reached *atomic.Int32
		// WantDelivered is whether Deliver must report the batch stored.
		WantDelivered bool
	}{{ // Test 0: A blocked address is refused before anything is sent.
		Name: "blocked address", URL: refused + "/collect", Reached: &direct, WantDelivered: false,
	}, { // Test 1: A redirect is a refused delivery, not a GET somewhere else.
		Name: "redirect", URL: srv.URL + "/hop", Reached: &landed, WantDelivered: false,
	}, { // Test 2: A collector on this host is reached.
		Name: "loopback collector", URL: srv.URL + "/collect", Reached: &direct,
		WantDelivered: true,
	}}
	// The cases share the arrival counters, so they run in order.
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			before := test.Reached.Load()
			err := NewHTTPSink(test.URL, nil, nil).Deliver(context.Background(), events)
			if delivered := err == nil; delivered != test.WantDelivered {
				t.Fatalf("%s: Deliver() error = %v, want delivered = %v", test.Name, err,
					test.WantDelivered)
			}
			want := int32(0)
			if test.WantDelivered {
				want = 1
			}
			if reached := test.Reached.Load() - before; reached != want {
				t.Errorf("%s: the batch reached the collector %d times, want %d", test.Name,
					reached, want)
			}
		})
	}
}
