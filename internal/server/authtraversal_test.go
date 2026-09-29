package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestPublicPrefixesCannotBeBorrowedByATraversal pins the end-to-end guarantee that no
// unauthenticated request reaches protected data by prefixing a guarded path with a public one.
//
// The gate's public carve-outs, the UI shell, healthz, readyz, the trust document, the SAML and
// OIDC handshakes, and the webhook prefix, are matched against the raw request path. So
// "/ui/../v1/users" satisfies the /ui/ carve-out and skips authentication entirely. What stops it
// today is the mux behind the gate: an unclean path is answered with a redirect rather than served,
// and the redirected request arrives clean and is gated normally. That means the refusal rests on a
// property of net/http's routing, not on anything this package states, and nothing pinned it.
//
// The assertion is deliberately about the response rather than about protects(): a redirect is an
// acceptable answer, a refusal is an acceptable answer, and a 200 carrying a guarded body is not.
func TestPublicPrefixesCannotBeBorrowedByATraversal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tokens := auth.NewMemStore()
	users := user.NewMemStore()
	admin, err := user.New("owner", "pw", user.RoleAdmin)
	if err != nil {
		t.Fatalf("user.New: %v", err)
	}
	if err := users.Save(ctx, admin); err != nil {
		t.Fatalf("Save user: %v", err)
	}
	// One token exists, so the gate is enforcing. It is never presented below.
	_, tok, err := auth.New("held-back")
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	tok.UserID = admin.ID
	if err := tokens.Save(ctx, tok); err != nil {
		t.Fatalf("Save token: %v", err)
	}
	handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
		WithTokens(tokens), WithUsers(users)).Handler()

	tests := []struct {
		Name   string
		Method string
		Path   string
	}{{ // Test 0: The UI shell is public, so its prefix is the widest borrow available.
		Name: "ui prefix into the account list", Method: http.MethodGet, Path: "/ui/../v1/users",
	}, { // Test 1: The same borrow aimed at the run list, which a viewer may read and a stranger may not.
		Name: "ui prefix into the run list", Method: http.MethodGet, Path: "/ui/../v1/runs",
	}, { // Test 2: The credential list names every secret on the install.
		Name: "ui prefix into credentials", Method: http.MethodGet, Path: "/ui/../v1/credentials",
	}, { // Test 3: Liveness is public and sits at the root, so it is borrowable the same way.
		Name: "healthz into the audit trail", Method: http.MethodGet, Path: "/healthz/../v1/audit",
	}, { // Test 4: The trust document is public for third parties with no account here.
		Name: "trust document into tokens", Method: http.MethodGet,
		Path: "/.well-known/../v1/tokens",
	}, { // Test 5: The webhook prefix skips the gate for a POST, which is the mutating case.
		Name: "hook prefix into run submission", Method: http.MethodPost,
		Path: "/hooks/whk_x/../../v1/runs",
	}, { // Test 6: The SAML handshake is public so the first sign-in can happen.
		Name: "saml prefix into the account list", Method: http.MethodGet,
		Path: "/auth/saml/../../v1/users",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(test.Method, "http://example.test/", nil)
			// Set after construction so the traversal survives: NewRequest parses and cleans a URL.
			r.URL.Path = test.Path
			r.RequestURI = test.Path
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)

			body := rec.Body.String()
			switch {
			case rec.Code == http.StatusUnauthorized, rec.Code == http.StatusForbidden,
				rec.Code == http.StatusNotFound, rec.Code >= 300 && rec.Code < 400:
				// Refused, or bounced to the clean path where the gate meets it.
			default:
				t.Fatalf("%s: %s %s answered %d, want a refusal or a redirect (body %s)",
					test.Name, test.Method, test.Path, rec.Code, strings.TrimSpace(body))
			}
			// A redirect must aim at the guarded path rather than serve it, and no answer here may
			// carry a payload from behind the gate.
			for _, leak := range []string{`"users"`, `"runs"`, `"credentials"`, `"entries"`, `"tokens"`} {
				if strings.Contains(body, leak) {
					t.Errorf("%s: answer carried %s from behind the gate: %s",
						test.Name, leak, strings.TrimSpace(body))
				}
			}
		})
	}
}
