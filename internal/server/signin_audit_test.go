package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// TestSignInPathsStayOutOfTheFailClosedAppend checks that every route a person signs in through is
// exempt from the audit append, and that ordinary mutations are not.
//
// Recording is fail closed on purpose: a mutation that cannot be written down does not happen. Sign
// in cannot live under that rule. It is reachable without a credential, so recording each attempt
// lets a stranger append to the chain without bound, and an unhealthy audit store then answers the
// login itself with a 503 and locks every user out of the install.
//
// SAML was the one sign in route that reached the chain. The OIDC callback is a GET and returns
// before the append either way, so dropping SAML from the exemption was not visible in the other
// SSO path. Its assertion consumer is a POST, it is the login in a SAML deployment, and it has no
// rate limiter in front of it.
func TestSignInPathsStayOutOfTheFailClosedAppend(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Path     string
		WantSkip bool
	}{ // Test 0 to 3: The sign in routes, both bare and under the version prefix.
		{"/auth/login", true},
		{"/v1/auth/login", true},
		{"/auth/oidc/callback", true},
		{"/auth/saml/acs", true},
		{"/v1/auth/saml/acs", true},
		{"/auth/saml/metadata", true},
		// Test 6 to 9: Everything else is recorded, including the unauthenticated routes.
		{"/v1/runs", false},
		{"/hooks/abc", false},
		{"/v1/users", false},
		{"/auth/tokens", false},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Path), func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
				"http://example.test"+test.Path, nil)
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			if got := isSignIn(req); got != test.WantSkip {
				if test.WantSkip {
					t.Errorf("%s is recorded, so an unhealthy audit store locks every user out "+
						"and a stranger can append to the chain without a credential", test.Path)
					return
				}
				t.Errorf("%s is exempt from the record, so a mutation happens with nothing "+
					"written down", test.Path)
			}
		})
	}
}
