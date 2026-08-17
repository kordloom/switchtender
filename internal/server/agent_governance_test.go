package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestAgentTokenIsGovernedAndAttributed proves the two guarantees the agent-governance wedge rests
// on: an agent token cannot reach the authority surface no matter what account it is bound to, and
// every action it does take is recorded in the chain under the agent identity, tied to the human it
// acts for. Together with the signed, verifiable chain, that is what makes "prove exactly what an
// agent did" a statement a third party can check rather than one the operator asserts.
func TestAgentTokenIsGovernedAndAttributed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tokens := auth.NewMemStore()
	users := user.NewMemStore()
	audits := audit.NewMemStore()

	// The agent is bound to an ADMIN account on purpose: the cap must hold regardless of the role
	// behind the token, so binding it to the most powerful account is the strongest test.
	admin, err := user.New("owner", "pw", user.RoleAdmin)
	if err != nil {
		t.Fatalf("user.New: %v", err)
	}
	if err := users.Save(ctx, admin); err != nil {
		t.Fatalf("Save user: %v", err)
	}
	plain, tok, err := auth.New("deploy-bot")
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	tok.UserID = admin.ID
	tok.Kind = auth.KindAgent
	if err := tokens.Save(ctx, tok); err != nil {
		t.Fatalf("Save token: %v", err)
	}

	sub := &fakeSubmitter{run: &run.Run{ID: "run_x"}}
	handler := New(run.NewMemStore(), sub, zap.NewNop(),
		WithTokens(tokens), WithUsers(users), WithAudit(audits)).Handler()

	call := func(method, path, body string) int {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		r.Header.Set("Authorization", "Bearer "+plain)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec.Code
	}

	// Capped at operator: the whole admin authority surface is denied even bound to an admin.
	denied := []struct{ method, path, body string }{
		{http.MethodGet, "/v1/users", ""},
		{http.MethodPost, "/v1/users", `{"username":"x","password":"y","role":"admin"}`},
		{http.MethodGet, "/v1/grants", ""},
		{http.MethodGet, "/v1/policies", ""},
		{http.MethodGet, "/v1/audit", ""},
		{http.MethodGet, "/v1/orgs", ""},
		{http.MethodPost, "/v1/credential-types", `{"name":"x"}`},
	}
	for _, d := range denied {
		if code := call(d.method, d.path, d.body); code != http.StatusForbidden {
			t.Errorf("agent %s %s = %d, want 403: an agent must not reach the authority surface",
				d.method, d.path, code)
		}
	}

	// It can still do operator work, which is the point of handing an agent a token.
	if code := call(http.MethodPost, "/v1/runs", `{"playbook":"site.yml","inventory":"h"}`); code != http.StatusAccepted {
		t.Errorf("agent launching a run = %d, want 202: an agent must still be able to work", code)
	}
	// The run itself carries who asked and how they authenticated, which is what lets a policy
	// treat an agent's request differently from a person's at the submission gate.
	if sub.gotRun == nil || sub.gotRun.Actor != "deploy-bot" || sub.gotRun.ActorType != "agent" {
		t.Errorf("submitted run actor = %+v, want deploy-bot (agent)", sub.gotRun)
	}

	// Every action it took is in the chain, attributed to the agent and to the human it acts for.
	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	var sawAgentRun bool
	for _, e := range chain {
		if e.Method == http.MethodPost && e.Path == "/v1/runs" {
			if e.ActorType != "agent" {
				t.Errorf("the run entry's actor_type = %q, want agent", e.ActorType)
			}
			if e.OnBehalfOf != "owner" {
				t.Errorf("the run entry's on_behalf_of = %q, want the human owner", e.OnBehalfOf)
			}
			if e.Actor != "deploy-bot" {
				t.Errorf("the run entry's actor = %q, want the token label", e.Actor)
			}
			sawAgentRun = true
		}
	}
	if !sawAgentRun {
		t.Error("the agent's run was not recorded in the chain, so it cannot be proven it happened")
	}
}
