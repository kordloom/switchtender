package cmd

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/spanbeat"
)

// TestBeatAnchorDialsTheAuthorityThroughTheGuard pins that serve's span beat anchoring reaches the
// timestamp authority through the dial guard. The authority's address is --anchor-tsa-url,
// operator configuration the server follows on its own network, and a plain client follows it
// anywhere: through the ambient proxy, and to any address at all.
//
// The authority stands behind the unspecified address, which the guard refuses and which a plain
// dial turns into this host, so a hook that reaches it is a hook with no guard. The loopback case
// shows the guard still lets an authority on this host through, so the refusal is the guard's and
// not a broken hook.
func TestBeatAnchorDialsTheAuthorityThroughTheGuard(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	tsa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(tsa.Close)
	anchors, ok := audit.NewMemStore().(audit.AnchorStore)
	if !ok {
		t.Fatal("the memory audit store no longer keeps anchors")
	}
	beat := spanbeat.AppendedBeat{At: time.Now(), Seq: 1, Hash: strings.Repeat("ab", 32), Beat: 1}

	tests := []struct {
		// Name labels the case in failure output.
		Name string
		// URL is the timestamp authority the hook is configured with.
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
			anchor := beatAnchorFunc(test.URL, "install-test", anchors)
			if err := anchor(context.Background(), beat); err == nil {
				t.Fatalf("%s: anchor() error = nil, want the request refused", test.Name)
			}
			reached := hits.Load()-before > 0
			if diff := cmp.Diff(test.WantReached, reached); diff != "" {
				t.Errorf("%s: authority reached mismatch (-want +got):\n%s", test.Name, diff)
			}
			saved, err := anchors.Anchors(context.Background(), 0)
			if err != nil {
				t.Fatalf("Anchors() error = %v", err)
			}
			if diff := cmp.Diff([]*audit.Anchor{}, saved, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s: saved anchors mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}
