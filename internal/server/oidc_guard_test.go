package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/user"
)

// oidcGuardAuth returns an OIDCAuth wired with just enough to reach the handshake checks. The code
// exchange and token verification are never reached by these cases, so the provider halves stay nil:
// every guard under test returns before any network call.
func oidcGuardAuth(t *testing.T) *OIDCAuth {
	t.Helper()
	return &OIDCAuth{
		users:       user.NewMemStore(),
		defaultRole: user.RoleViewer,
		signKey:     []byte("test-handshake-key"),
		log:         zap.NewNop(),
	}
}

// signedHandshake returns a request carrying a valid handshake cookie for the given values, the
// state a browser is in when it comes back from the provider.
func signedHandshake(t *testing.T, o *OIDCAuth, state, nonce, query string) *http.Request {
	t.Helper()
	rec := httptest.NewRecorder()
	o.setHandshake(rec, state, nonce, "test-verifier")
	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?"+query, nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	return req
}

// TestOIDCCallbackRefusesABadHandshake drives the real callback handler so the guards that protect
// sign-in are executed, not merely present.
//
// The state check is the CSRF defense: without it an attacker completes their own authorization at
// the provider and feeds the resulting code to a victim's browser, signing the victim into the
// attacker's account. The missing-cookie case is the same attack with no handshake at all. Both
// return before any network call, so this test needs no provider.
//
// It exists because these guards had no test that executed them. Deleting either one left the whole
// suite green, so the code was correct and the wiring was free to be removed by anyone refactoring.
func TestOIDCCallbackRefusesABadHandshake(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Cookie   bool
		State    string
		Query    string
		WantText string
	}{
		{
			Name: "state does not match the handshake", Cookie: true, State: "real-state",
			Query: "state=attacker-state&code=abc", WantText: "state mismatch",
		},
		{
			Name: "no handshake cookie at all", Cookie: false,
			Query: "state=anything&code=abc", WantText: "expired",
		},
		{
			Name: "state absent from the callback", Cookie: true, State: "real-state",
			Query: "code=abc", WantText: "state mismatch",
		},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			o := oidcGuardAuth(t)
			var req *http.Request
			if test.Cookie {
				req = signedHandshake(t, o, test.State, "real-nonce", test.Query)
			} else {
				req = httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?"+test.Query, nil)
			}
			rec := httptest.NewRecorder()
			o.callback(rec, req)

			if rec.Code != http.StatusFound {
				t.Fatalf("status = %d, want a redirect back to sign-in", rec.Code)
			}
			location := rec.Header().Get("Location")
			if !strings.Contains(location, "/ui/login") {
				t.Errorf("redirect = %q, want the sign-in page", location)
			}
			// The message rides in a query-encoded fragment, so spaces arrive as plus signs.
			if !strings.Contains(location, strings.ReplaceAll(test.WantText, " ", "+")) {
				t.Errorf("redirect = %q, want it to name %q", location, test.WantText)
			}
			// A refused sign-in must not mint anything. Reaching the account store at all would mean
			// the handler continued past the guard.
			accounts, err := o.users.List(req.Context())
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if len(accounts) != 0 {
				t.Errorf("a refused sign-in provisioned %d account(s)", len(accounts))
			}
		})
	}
}

// TestOIDCHandshakeCookieIsTamperEvident checks the handshake cannot be rewritten by the browser it
// is handed to. The cookie carries the state and nonce the callback compares against, so a forgeable
// one would let a caller choose both sides of the comparison and pass it trivially.
func TestOIDCHandshakeCookieIsTamperEvident(t *testing.T) {
	t.Parallel()
	o := oidcGuardAuth(t)
	req := signedHandshake(t, o, "real-state", "real-nonce", "state=real-state&code=abc")

	// The handshake reads back intact before tampering, so the negative case below means something.
	if _, err := o.readHandshake(req); err != nil {
		t.Fatalf("readHandshake() on an untouched cookie error = %v", err)
	}

	tampered := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?state=x&code=abc", nil)
	for _, c := range req.Cookies() {
		// Swap the payload for one naming a state the caller chose, keeping the original signature.
		_, sig, _ := strings.Cut(c.Value, ".")
		forged := "YXR0YWNrZXJ8bm9uY2V8dmVyaWZpZXJ8OTk5OTk5OTk5OQ." + sig
		tampered.AddCookie(&http.Cookie{Name: c.Name, Value: forged})
	}
	if _, err := o.readHandshake(tampered); err == nil {
		t.Error("a rewritten handshake cookie was accepted, so its state and nonce are attacker chosen")
	}
}
