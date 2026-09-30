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

// TestPagesAskForSignInExactlyWhereTheAPIRefuses pins the flag the UI script reads against the gate
// it has to agree with. A page that carries it sends a signed-out visitor to sign in before asking
// for any data, which is what keeps a first visit from logging a 401 per request. Carried where
// the API runs open, it would bounce every visitor to a sign-in page they cannot use, so the test
// checks both halves on the same install: what the page says, and what the API does.
func TestPagesAskForSignInExactlyWhereTheAPIRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Token    bool
		Account  bool
		Enforced bool
		Want     bool
	}{{ // Test 0: A fresh install runs open, so nobody is sent anywhere.
		Name: "fresh install", Want: false,
	}, { // Test 1: A token turns authentication on.
		Name: "a token", Token: true, Want: true,
	}, { // Test 2: So does an account.
		Name: "an account", Account: true, Want: true,
	}, { // Test 3: Single sign-on authenticates before anybody has signed in and been provisioned.
		Name: "single sign-on before the first sign-in", Enforced: true, Want: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			tokens := auth.NewMemStore()
			users := user.NewMemStore()
			if test.Token {
				_, tok, err := auth.New("ci")
				if err != nil {
					t.Fatalf("auth.New() error = %v", err)
				}
				if err := tokens.Save(ctx, tok); err != nil {
					t.Fatalf("Save(token) error = %v", err)
				}
			}
			if test.Account {
				account := &user.User{ID: "user_1", Username: "ops", Role: user.RoleAdmin}
				if err := users.Save(ctx, account); err != nil {
					t.Fatalf("Save(user) error = %v", err)
				}
			}
			opts := []Option{WithTokens(tokens), WithUsers(users)}
			if test.Enforced {
				opts = append(opts, WithEnforcedAuth())
			}
			handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), opts...).Handler()

			page := httptest.NewRecorder()
			handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/ui/", nil))
			says := strings.Contains(page.Body.String(), `data-signin="required"`)
			api := httptest.NewRecorder()
			handler.ServeHTTP(api, httptest.NewRequest(http.MethodGet, "/v1/runs", nil))
			refuses := api.Code == http.StatusUnauthorized
			if says != test.Want || refuses != test.Want {
				t.Errorf("%s: the page asks for sign-in = %v and the API refuses a signed-out read = %v, "+
					"want both %v", test.Name, says, refuses, test.Want)
			}
		})
	}
}
