package cmd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestMCPRefusesToStartOnAnAdminToken proves the mcp command probes the token's authority before it
// serves anything, and refuses when the probe says the token can administer the install. An agent on
// an admin token could approve its own runs, so the refusal is the whole point of the check; a
// command that started anyway would hand that authority to the agent with only a doc comment
// standing in the way.
func TestMCPRefusesToStartOnAnAdminToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the token authority the server reports.
		Name string
		// Status is what the account endpoint answers the probe with.
		Status int
		// WantError is a fragment the returned error must carry.
		WantError string
	}{{ // Test 0: Listing accounts succeeds, so the token is an admin token and the command refuses.
		Name: "admin token", Status: http.StatusOK,
		WantError: "refusing to serve an agent on an admin token",
	}, { // Test 1: A rejected token is reported as itself, not read as either verdict.
		Name: "rejected token", Status: http.StatusUnauthorized,
		WantError: "the token was rejected by the server",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			// Not parallel: the mcp command reads package-level flag variables.
			var probes atomic.Int64
			var probedPath, probedAuth atomic.Value
			probedPath.Store("")
			probedAuth.Store("")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				probes.Add(1)
				probedPath.Store(r.URL.Path)
				probedAuth.Store(r.Header.Get("Authorization"))
				w.WriteHeader(test.Status)
			}))
			defer srv.Close()

			mcpServer, mcpToken = srv.URL, "st_agenttoken"
			mcpTimeout, mcpAllowAdminToken = 5*time.Second, false
			t.Cleanup(func() {
				mcpServer, mcpToken, mcpTimeout, mcpAllowAdminToken = "", "", 0, false
			})

			err := runMCP(testCommand(), nil)
			if err == nil {
				t.Fatalf("test %d: runMCP() error = nil, want it to stop before serving", testNum)
			}
			if !strings.Contains(err.Error(), test.WantError) {
				t.Fatalf("test %d: runMCP() error = %q, want it to carry %q",
					testNum, err, test.WantError)
			}
			// The probe is the side effect that proves the authority check ran rather than the
			// command failing for some other reason on its way to serving.
			if got := probes.Load(); got != 1 {
				t.Errorf("test %d: server saw %d request(s), want exactly the authority probe", testNum, got)
			}
			if got := probedPath.Load().(string); got != "/v1/users" {
				t.Errorf("test %d: probed %q, want the account endpoint", testNum, got)
			}
			if got := probedAuth.Load().(string); got != "Bearer st_agenttoken" {
				t.Errorf("test %d: probe authorization = %q, want the agent's token", testNum, got)
			}
		})
	}
}
