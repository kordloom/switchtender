package demo

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
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
)

// TestSeedAnchorsDialsTheAuthorityThroughTheGuard pins that the demo seeder reaches the timestamp
// authority through the dial guard. The authority's address is the demo's --anchor-tsa,
// configuration the demo follows on its own network, and a plain client follows it anywhere:
// through the ambient proxy, and to any address at all.
//
// The authority stands behind the unspecified address, which the guard refuses and which a plain
// dial turns into this host, so a seeder that reaches it is a seeder with no guard. The loopback
// case shows the guard still lets an authority on this host through, so the refusal is the guard's
// and not a seeder that never asks.
func TestSeedAnchorsDialsTheAuthorityThroughTheGuard(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	tsa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(tsa.Close)

	tests := []struct {
		// Name labels the case in failure output.
		Name string
		// URL is the timestamp authority the seeder is configured with.
		URL string
		// WantReached is whether the seeder's requests must arrive at the authority.
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
			ctx := context.Background()
			store := audit.NewMemStore()
			anchors, ok := store.(audit.AnchorStore)
			if !ok {
				t.Fatal("the memory audit store no longer keeps anchors")
			}
			if err := store.Append(ctx, &audit.Entry{
				Actor: "seed", Method: "POST", Path: "/api/demo", At: time.Now().UTC(),
			}); err != nil {
				t.Fatalf("Append() error = %v", err)
			}
			before := hits.Load()
			seedAnchors(ctx, Deps{Audit: store, AnchorTSA: test.URL, InstallID: "install-demo"},
				zap.NewNop())
			reached := hits.Load()-before > 0
			if diff := cmp.Diff(test.WantReached, reached); diff != "" {
				t.Errorf("%s: authority reached mismatch (-want +got):\n%s", test.Name, diff)
			}
			saved, err := anchors.Anchors(ctx, 0)
			if err != nil {
				t.Fatalf("Anchors() error = %v", err)
			}
			if diff := cmp.Diff([]*audit.Anchor{}, saved, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s: saved anchors mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}
