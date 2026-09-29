package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/user"
)

// TestRoleForRefusesAnOwnedTokenWithoutAccounts proves a token naming an owner is denied, not
// promoted, when there is no account store to read the owner's role from.
//
// The missing store used to resolve to admin. A token minted for a viewer would then have carried
// admin rights for as long as the store was absent, and the reply would have looked like any other
// authorized one. An unreadable role is the one case that must not move upward.
func TestRoleForRefusesAnOwnedTokenWithoutAccounts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		UserID   string
		WantRole user.Role
		Want     error
	}{{ // Test 0: A token bound to an account with no accounts wired is refused.
		Name: "owned token", UserID: "user_1", WantRole: "", Want: errNoAccounts,
	}, { // Test 1: An unscoped command-line token still carries admin.
		Name: "unscoped token", UserID: "", WantRole: user.RoleAdmin, Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			g := &authGate{log: zap.NewNop()}
			role, boundUser, err := g.roleFor(context.Background(),
				&auth.Token{ID: "tok_1", UserID: test.UserID})
			if !errors.Is(err, test.Want) {
				t.Fatalf("roleFor() error = %v, want %v", err, test.Want)
			}
			if role != test.WantRole {
				t.Errorf("roleFor() role = %q, want %q", role, test.WantRole)
			}
			// Neither case resolves an account, so neither names one to record as the delegation.
			if boundUser != "" {
				t.Errorf("roleFor() bound account = %q, want empty", boundUser)
			}
		})
	}
}

// TestGateDeniesAnOwnedTokenWithoutAccounts drives the same case through the middleware, so the
// escalation is proved closed where it would have been exploited: an admin-only path answered with
// a token whose account cannot be read.
func TestGateDeniesAnOwnedTokenWithoutAccounts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tokens := auth.NewMemStore()
	plain, tok, err := auth.New("bound")
	if err != nil {
		t.Fatalf("auth.New() error = %v", err)
	}
	tok.UserID = "user_1"
	if err := tokens.Save(ctx, tok); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// users is nil, which is the whole point: the owner's role cannot be read.
	gate := &authGate{tokens: tokens, log: zap.NewNop(), authz: &authorizer{}}
	handler := gate.wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/users", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("admin-only path with an unreadable account answered %d, want %d",
			rec.Code, http.StatusUnauthorized)
	}
}
