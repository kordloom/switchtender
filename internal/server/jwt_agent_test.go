package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestFederatedAgentTokenIsHeldToTheAgentCeiling checks that a token the directory marks as an
// agent's gets the agent ceiling, whatever role its group maps to, and that its run is recorded
// with the agent actor type.
func TestFederatedAgentTokenIsHeldToTheAgentCeiling(t *testing.T) {
	t.Parallel()
	const agentTok, personTok = "a.g.ent", "p.er.son"
	admin := func() *user.User { return &user.User{Username: "svc-deploy", Role: user.RoleAdmin} }
	jwtAuth := fakeJWT{
		users:  map[string]*user.User{agentTok: admin(), personTok: admin()},
		agents: map[string]bool{agentTok: true},
	}
	users := user.NewMemStore()
	tokens := auth.NewMemStore()
	_, bootstrap, err := auth.New("bootstrap")
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	if err := tokens.Save(context.Background(), bootstrap); err != nil {
		t.Fatalf("tokens.Save: %v", err)
	}
	audits := audit.NewMemStore()
	handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_agent"}}, zap.NewNop(),
		WithTokens(tokens), WithJWT(jwtAuth), WithAudit(audits), WithUsers(users)).Handler()

	// Test 0: The agent is refused the admin-only user route its group would reach.
	agentReq := httptest.NewRequest(http.MethodPost, "/v1/users",
		strings.NewReader(`{"username":"new-admin","password":"correct-horse-battery","role":"admin"}`))
	agentReq.Header.Set("Authorization", "Bearer "+agentTok)
	agentRec := httptest.NewRecorder()
	handler.ServeHTTP(agentRec, agentReq)
	if agentRec.Code != http.StatusForbidden {
		t.Fatalf("agent POST /v1/users status = %d, want 403", agentRec.Code)
	}

	// Test 1: The same account, without the agent mark, reaches that route.
	personReq := httptest.NewRequest(http.MethodPost, "/v1/users",
		strings.NewReader(`{"username":"new-admin","password":"correct-horse-battery","role":"admin"}`))
	personReq.Header.Set("Authorization", "Bearer "+personTok)
	personRec := httptest.NewRecorder()
	handler.ServeHTTP(personRec, personReq)
	if personRec.Code != http.StatusCreated {
		t.Fatalf("person POST /v1/users status = %d, want 201: %s", personRec.Code, personRec.Body.String())
	}

	// Test 2: The agent's run is recorded with the agent actor type.
	runReq := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(`{"playbook":"p.yml"}`))
	runReq.Header.Set("Authorization", "Bearer "+agentTok)
	handler.ServeHTTP(httptest.NewRecorder(), runReq)
	var entries []*audit.Entry
	for range 50 {
		if entries, _ = audits.List(context.Background(), 10); len(entries) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(entries) == 0 {
		t.Fatal("no audit entry for the agent's run")
	}
	if got := entries[0].ActorType; got != actorTypeAgent {
		t.Errorf("actor type = %q, want %q", got, actorTypeAgent)
	}
}
