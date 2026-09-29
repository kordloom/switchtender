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

// TestTokenEndpointsMintListAndRevoke covers the gap that made the agent story unreachable on any
// install an operator cannot get a shell on: tokens, agent tokens above all, could only be minted,
// listed, and revoked from the command line against the database file. A hosted install, a container
// with no shell, or an admin working from a browser had no way to issue an agent a credential or to
// take one back.
func TestTokenEndpointsMintListAndRevoke(t *testing.T) {
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
	operator, err := user.New("casey", "pw", user.RoleOperator)
	if err != nil {
		t.Fatalf("user.New: %v", err)
	}
	if err := users.Save(ctx, operator); err != nil {
		t.Fatalf("Save user: %v", err)
	}
	adminPlain, adminTok, err := auth.New("cli")
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	adminTok.UserID = admin.ID
	if err := tokens.Save(ctx, adminTok); err != nil {
		t.Fatalf("Save token: %v", err)
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

	// Test 0: An agent token is minted for a named account, and its plaintext is returned once.
	rec := call(http.MethodPost, "/v1/tokens", adminPlain,
		`{"name":"deploy-bot","kind":"agent","username":"casey"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint agent token = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var minted struct {
		ID    string `json:"id"`
		Token string `json:"token"`
		Kind  string `json:"kind"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatalf("decode mint: %v", err)
	}
	if minted.Token == "" || minted.ID == "" {
		t.Fatalf("mint response = %s, want an id and the plaintext once", rec.Body.String())
	}
	if minted.Kind != auth.KindAgent {
		t.Errorf("minted kind = %q, want agent", minted.Kind)
	}

	// The minted token works and is capped to operator, the agent ceiling, despite nothing about the
	// request saying so.
	rec = call(http.MethodGet, "/v1/auth/me", minted.Token, "")
	var me map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatalf("decode auth/me: %v", err)
	}
	if me["actor_type"] != "agent" || me["role"] != "operator" {
		t.Errorf("minted agent identity = %v, want an operator-capped agent", me)
	}

	// Test 1: The list shows it, with no secret in the body.
	rec = call(http.MethodGet, "/v1/tokens", adminPlain, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list tokens = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), minted.Token) {
		t.Error("the token list returns the secret itself, so reading the list hands out credentials")
	}
	var list struct {
		Tokens []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	var found bool
	for _, tk := range list.Tokens {
		if tk.ID == minted.ID && tk.Name == "deploy-bot" && tk.Kind == auth.KindAgent {
			found = true
		}
	}
	if !found {
		t.Errorf("the minted token is not in the list: %s", rec.Body.String())
	}

	// Test 2: An agent token cannot mint tokens. The whole point of the cap is that a machine
	// principal cannot widen its own access, and minting is the widest thing there is.
	rec = call(http.MethodPost, "/v1/tokens", minted.Token, `{"name":"second","username":"casey"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("agent minting a token = %d, want 403: an agent can issue itself credentials",
			rec.Code)
	}

	// Test 3: An unscoped token, which acts as admin with no account behind it, is not something the
	// API mints. It would be an unattributable credential handed out over the network.
	rec = call(http.MethodPost, "/v1/tokens", adminPlain, `{"name":"nobody"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("mint unbound token = %d, want 400", rec.Code)
	}
	// The refusal has to be the one about binding. Falling through to the account lookup would also
	// answer 400, but it would tell the caller their account does not exist rather than that a token
	// needs an account behind it, and it would mean the rule is enforced by accident.
	if !strings.Contains(rec.Body.String(), "username is required") {
		t.Errorf("unbound mint refusal = %s, want the rule about binding a token to an account",
			rec.Body.String())
	}

	// Test 4: An agent token must name the account it acts for.
	rec = call(http.MethodPost, "/v1/tokens", adminPlain, `{"name":"bot","kind":"agent"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("mint unbound agent token = %d, want 400", rec.Code)
	}
	// Nothing was minted by either refusal. A 400 with a token in the database would be worse than a
	// 201, because the credential would exist and nobody would know to revoke it.
	stored, lerr := tokens.List(ctx)
	if lerr != nil {
		t.Fatalf("List tokens: %v", lerr)
	}
	for _, tk := range stored {
		if tk.Name == "nobody" || tk.Name == "bot" {
			t.Errorf("a refused mint stored the token %q anyway", tk.Name)
		}
	}

	// Test 5: The list is not viewer material. It names every credential with access to this install
	// and which account each acts as, which is a map of what is worth stealing.
	viewer, verr := user.New("reader", "pw", user.RoleViewer)
	if verr != nil {
		t.Fatalf("user.New: %v", verr)
	}
	if err := users.Save(ctx, viewer); err != nil {
		t.Fatalf("Save user: %v", err)
	}
	viewerPlain, viewerTok, verr := auth.New("reader-token")
	if verr != nil {
		t.Fatalf("auth.New: %v", verr)
	}
	viewerTok.UserID = viewer.ID
	if err := tokens.Save(ctx, viewerTok); err != nil {
		t.Fatalf("Save token: %v", err)
	}
	if rec := call(http.MethodGet, "/v1/tokens", viewerPlain, ""); rec.Code != http.StatusForbidden {
		t.Errorf("viewer listing tokens = %d, want 403", rec.Code)
	}

	// Test 6: Revoking stops the token working.
	rec = call(http.MethodDelete, "/v1/tokens/"+minted.ID, adminPlain, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke token = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := call(http.MethodGet, "/v1/auth/me", minted.Token, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked token still authenticates: %d", rec.Code)
	}
	if rec := call(http.MethodDelete, "/v1/tokens/"+minted.ID, adminPlain, ""); rec.Code != http.StatusNotFound {
		t.Errorf("revoking a gone token = %d, want 404", rec.Code)
	}
}
