package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestProtectsSAMLPaths verifies the token gate exempts the SAML handshake and still protects
// everything else under /auth/saml.
func TestProtectsSAMLPaths(t *testing.T) {
	t.Parallel()
	g := &authGate{}
	tests := []struct {
		Method string
		Path   string
		Want   bool
	}{{ // Test 0: The login redirect runs before the user has a token.
		Method: http.MethodGet, Path: "/auth/saml/login", Want: false,
	}, { // Test 1: The metadata read is public for IdP registration.
		Method: http.MethodGet, Path: "/auth/saml/metadata", Want: false,
	}, { // Test 2: The IdP posts the assertion without a token.
		Method: http.MethodPost, Path: "/auth/saml/acs", Want: false,
	}, { // Test 3: Any other SAML post stays protected.
		Method: http.MethodPost, Path: "/auth/saml/login", Want: true,
	}, { // Test 4: An API path is unaffected.
		Method: http.MethodGet, Path: "/v1/runs", Want: true,
	}}
	for _, test := range tests {
		req := httptest.NewRequest(test.Method, test.Path, nil)
		if got := g.protects(req); got != test.Want {
			t.Errorf("protects(%s %s) = %v, want %v", test.Method, test.Path, got, test.Want)
		}
	}
}
