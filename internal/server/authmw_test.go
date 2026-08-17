package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestAuthMeAnswersTheResolvedIdentity proves /v1/auth/me tells a caller who the server resolved
// it to be: the bound account's role for a scoped token, the agent cap applied for an agent token,
// and an open answer when the API is not enforcing. The UI gates its controls with this instead of
// treating every token session as an admin.
func TestAuthMeAnswersTheResolvedIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tokens := auth.NewMemStore()
	users := user.NewMemStore()

	viewer, err := user.New("reader", "pw", user.RoleViewer)
	if err != nil {
		t.Fatalf("user.New: %v", err)
	}
	if err := users.Save(ctx, viewer); err != nil {
		t.Fatalf("Save user: %v", err)
	}
	admin, err := user.New("owner", "pw", user.RoleAdmin)
	if err != nil {
		t.Fatalf("user.New: %v", err)
	}
	if err := users.Save(ctx, admin); err != nil {
		t.Fatalf("Save user: %v", err)
	}
	viewerPlain, viewerTok, err := auth.New("reader-token")
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	viewerTok.UserID = viewer.ID
	if err := tokens.Save(ctx, viewerTok); err != nil {
		t.Fatalf("Save token: %v", err)
	}
	agentPlain, agentTok, err := auth.New("deploy-bot")
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	agentTok.UserID = admin.ID
	agentTok.Kind = auth.KindAgent
	if err := tokens.Save(ctx, agentTok); err != nil {
		t.Fatalf("Save token: %v", err)
	}

	handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
		WithTokens(tokens), WithUsers(users)).Handler()
	me := func(bearer string) map[string]any {
		r := httptest.NewRequest(http.MethodGet, "/v1/auth/me", nil)
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("auth/me status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode auth/me: %v", err)
		}
		return out
	}

	got := me(viewerPlain)
	if got["role"] != "viewer" || got["name"] != "reader-token" || got["actor_type"] != "token" {
		t.Errorf("viewer token identity = %v, want reader-token viewer token", got)
	}
	// The agent cap applies before the answer, so an agent bound to an admin reads as operator.
	got = me(agentPlain)
	if got["role"] != "operator" || got["actor_type"] != "agent" {
		t.Errorf("agent token identity = %v, want operator agent", got)
	}

	// An open install has nobody to be, and says so rather than inventing a role.
	open := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop()).Handler()
	rec := httptest.NewRecorder()
	open.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/auth/me", nil))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode open auth/me: %v", err)
	}
	if out["open"] != true {
		t.Errorf("open install identity = %v, want open true", out)
	}
}
