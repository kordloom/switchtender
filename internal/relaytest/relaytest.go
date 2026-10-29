// Package relaytest holds what relay tests share across packages: a relay server over a policy
// store with a client dialing it, so a test reads policies the way a relay worker does.
package relaytest

import (
	"net/http/httptest"
	"testing"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// workerToken is the token the relay server issues to its one pool and the client presents.
const workerToken = "relaytest-worker-secret"

// NewPolicyRelay stands up a relay server over the given policy store and returns a PolicyClient
// dialing it, so a test reads policies the way a relay worker does.
func NewPolicyRelay(t *testing.T, policies policy.Store) *relay.PolicyClient {
	t.Helper()
	ts := httptest.NewServer(relay.NewHandler(run.NewMemStore(), relay.SinglePool(workerToken), nil,
		policies, nil))
	t.Cleanup(ts.Close)
	return relay.NewPolicyClient(relay.NewHTTPTransport(ts.URL, workerToken, ts.Client()))
}
