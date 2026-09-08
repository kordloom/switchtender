package safedial

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"
)

// TestEachConstructorCarriesItsOwnRule dials a real listener on the loopback interface through
// every client and transport this package builds, and requires each to reach or refuse it according
// to the rule it is named for.
//
// The rules themselves are pinned elsewhere against addresses. What is pinned here is the wiring
// between a constructor and the rule it installs, which no address-level test can see: if
// OffHostClient were ever built with Control instead of ControlOffHost, this server would become
// reachable by any URL a run may carry, and every existing test would still pass. The two rules
// differ on exactly one address family, loopback, so that is what this dials.
func TestEachConstructorCarriesItsOwnRule(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	tests := []struct {
		Client    *http.Client
		Name      string
		WantReach bool
	}{{ // Test 0: The administrator rule reaches a local agent, which is a real deployment.
		Name: "Client reaches loopback", Client: Client(5 * time.Second), WantReach: true,
	}, { // Test 1: The caller-supplied rule refuses this server itself.
		Name: "OffHostClient refuses loopback", Client: OffHostClient(5 * time.Second),
		WantReach: false,
	}, { // Test 2: A caller assembling its own client from the admin transport reaches loopback.
		Name:      "Transport reaches loopback",
		Client:    &http.Client{Transport: Transport(), Timeout: 5 * time.Second},
		WantReach: true,
	}, { // Test 3: A caller assembling its own client from the off-host transport does not.
		Name:      "OffHostTransport refuses loopback",
		Client:    &http.Client{Transport: OffHostTransport(), Timeout: 5 * time.Second},
		WantReach: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			resp, err := test.Client.Get(server.URL)
			if resp != nil {
				resp.Body.Close()
			}
			if reached := err == nil; reached != test.WantReach {
				t.Fatalf("%s: reached = %v (err %v), want reached = %v",
					test.Name, reached, err, test.WantReach)
			}
		})
	}
}

// TestControlHooksMatchTheirRules pins the two dialer hooks directly against the rules they are
// meant to apply, so a hook wired to the wrong rule is caught even if no constructor uses it yet.
func TestControlHooksMatchTheirRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Hook       func(string, string, syscall.RawConn) error
		Name       string
		Address    string
		WantRefuse bool
	}{{ // Test 0: The administrator hook allows a local agent.
		Name: "Control allows loopback", Hook: Control, Address: "127.0.0.1:8200",
		WantRefuse: false,
	}, { // Test 1: The administrator hook still refuses the metadata endpoint.
		Name: "Control refuses metadata", Hook: Control, Address: "169.254.169.254:80",
		WantRefuse: true,
	}, { // Test 2: The caller-supplied hook refuses this server itself.
		Name: "ControlOffHost refuses loopback", Hook: ControlOffHost, Address: "127.0.0.1:8200",
		WantRefuse: true,
	}, { // Test 3: The caller-supplied hook still reaches an ordinary internal target.
		Name: "ControlOffHost allows a private address", Hook: ControlOffHost,
		Address: "10.0.0.5:80", WantRefuse: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := test.Hook("tcp", test.Address, nil)
			if refused := err != nil; refused != test.WantRefuse {
				t.Errorf("%s: %s(%q) error = %v, want refused = %v",
					test.Name, test.Name, test.Address, err, test.WantRefuse)
			}
		})
	}
}

// TestNeitherClientFollowsARedirect pins that a checked URL cannot become an unchecked one by
// answering with a redirect. Both clients this package hands out must stop at the first response.
func TestNeitherClientFollowsARedirect(t *testing.T) {
	t.Parallel()
	var target *httptest.Server
	target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hop" {
			http.Redirect(w, r, target.URL+"/landed", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(target.Close)

	resp, err := Client(5 * time.Second).Get(target.URL + "/hop")
	if err != nil {
		t.Fatalf("Client.Get(/hop) error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("Client followed the redirect: status = %d, want %d",
			resp.StatusCode, http.StatusFound)
	}
}
