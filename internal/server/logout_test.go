package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestSignInIsIdentifiableAndRevocable covers two halves of one gap. A browser sign-in mints a
// long-lived token row, and nothing marked it as a session: every action a person took at the
// keyboard was attributed on the chain as actor_type "token", indistinguishable from a scripted
// call, which is the one distinction the identity stage of the boundary exists to make. And there
// was no way to end a session at all: signing out could only drop the browser's copy, leaving the
// token accepted for its full thirty days by anyone holding it.
func TestSignInIsIdentifiableAndRevocable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tokens := auth.NewMemStore()
	users := user.NewMemStore()

	operator, err := user.New("casey", "correct-horse", user.RoleOperator)
	if err != nil {
		t.Fatalf("user.New: %v", err)
	}
	if err := users.Save(ctx, operator); err != nil {
		t.Fatalf("Save user: %v", err)
	}
	handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
		WithTokens(tokens), WithUsers(users)).Handler()

	call := func(method, path, bearer, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec
	}

	rec := call(http.MethodPost, "/v1/auth/login", "",
		`{"username":"casey","password":"correct-horse"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("sign in status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var session struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode sign in: %v", err)
	}

	// Test 0: The chain and the interface both learn that a person signed in, not that a token called.
	var me map[string]any
	rec = call(http.MethodGet, "/v1/auth/me", session.Token, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatalf("decode auth/me: %v", err)
	}
	if me["actor_type"] != "session" {
		t.Errorf("signed-in actor type = %v, want session: a person at a browser is recorded as a "+
			"scripted token call", me["actor_type"])
	}
	if me["name"] != "casey" {
		t.Errorf("signed-in name = %v, want casey", me["name"])
	}

	// Test 1: Signing out revokes the session server-side, so the token it held stops working.
	rec = call(http.MethodPost, "/v1/auth/logout", session.Token, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("sign out status = %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := call(http.MethodGet, "/v1/auth/me", session.Token, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("reused session after sign out = %d, want 401: the session was not revoked", rec.Code)
	}

	// Test 2: An API token used to browse is not destroyed by a sign out. It belongs to whatever else
	// holds it, a scheduled job most likely, and a browser tab must not revoke that.
	plain, tok, err := auth.New("laptop")
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	tok.UserID = operator.ID
	if err := tokens.Save(ctx, tok); err != nil {
		t.Fatalf("Save token: %v", err)
	}
	if rec := call(http.MethodPost, "/v1/auth/logout", plain, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("API token sign out status = %d, want 204", rec.Code)
	}
	if rec := call(http.MethodGet, "/v1/auth/me", plain, ""); rec.Code != http.StatusOK {
		t.Errorf("API token after sign out = %d, want 200: a browser sign out revoked a token that "+
			"other things hold", rec.Code)
	}

	// Test 3: Sign out needs a caller. An anonymous one has no session to end.
	if rec := call(http.MethodPost, "/v1/auth/logout", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous sign out = %d, want 401", rec.Code)
	}
}
