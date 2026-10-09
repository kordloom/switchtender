package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// identityAnswer is the part of the /v1/auth/me answer these tests read.
type identityAnswer struct {
	// Name is the audit name the credential was minted with.
	Name string `json:"name"`
	// Account is the current username of the account the credential is bound to.
	Account string `json:"account"`
}

// TestAuthMeNamesTheAccountBehindTheSession proves /v1/auth/me carries the current username of the
// account a credential is bound to, beside the audit name the credential was minted with. A
// session keeps the name it signed in with, since that is what the chain records on everything it
// does, and the account field is what lets a page name a renamed account by its current username.
func TestAuthMeNamesTheAccountBehindTheSession(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantIdentity identityAnswer
		Rename       string
		Bound        bool
	}{{ // Test 0: A session bound to an account carries the account's username.
		Bound: true, WantIdentity: identityAnswer{Name: "casey", Account: "casey"},
	}, { // Test 1: A renamed account keeps its audit name and carries its new username.
		Bound: true, Rename: "casey-renamed",
		WantIdentity: identityAnswer{Name: "casey", Account: "casey-renamed"},
	}, { // Test 2: A token bound to no account names none.
		WantIdentity: identityAnswer{Name: "casey"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			tokens := auth.NewMemStore()
			users := user.NewMemStore()
			account, err := user.New("casey", "pw", user.RoleAdmin)
			if err != nil {
				t.Fatalf("user.New: %v", err)
			}
			if err := users.Save(ctx, account); err != nil {
				t.Fatalf("Save user: %v", err)
			}
			plain, tok, err := auth.New("casey")
			if err != nil {
				t.Fatalf("auth.New: %v", err)
			}
			if test.Bound {
				tok.UserID = account.ID
				tok.Kind = auth.KindSession
			}
			if err := tokens.Save(ctx, tok); err != nil {
				t.Fatalf("Save token: %v", err)
			}
			if test.Rename != "" {
				account.Username = test.Rename
				if err := users.Save(ctx, account); err != nil {
					t.Fatalf("rename user: %v", err)
				}
			}
			handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
				WithTokens(tokens), WithUsers(users)).Handler()
			r := httptest.NewRequest(http.MethodGet, "/v1/auth/me", nil)
			r.Header.Set("Authorization", "Bearer "+plain)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			if rec.Code != http.StatusOK {
				t.Fatalf("auth/me status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
			}
			var got identityAnswer
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode auth/me: %v", err)
			}
			if diff := cmp.Diff(test.WantIdentity, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("identity mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
