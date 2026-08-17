package server

import (
	"context"
	"encoding/json"
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

// TestAnActorCanReadTheEvidenceForItsOwnRun covers a tool the product advertises and could never
// execute. The MCP server refuses to start on a token with admin rights, deliberately, so an agent
// always holds an operator-bound token. The evidence dossier is admin-only. So get_run_evidence, the
// tool whose whole purpose is letting an agent show what its change actually did, answered 403 every
// time it was called, on every install.
//
// The rule that makes it work without widening anything: a caller may read the evidence for a run they
// themselves asked for. The dossier for one run holds that run's spec, its decisions, its outcomes, and
// the chain entries over it. A requester already knows they asked; what they gain is the ability to
// prove it, which is the accountability the agent identity exists for. Somebody else's run stays admin
// ground, and a viewer is refused either way.
func TestAnActorCanReadTheEvidenceForItsOwnRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	users := user.NewMemStore()
	tokens := auth.NewMemStore()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()

	agentAccount, err := user.New("bot-owner", "pw", user.RoleOperator)
	if err != nil {
		t.Fatalf("user.New: %v", err)
	}
	if err := users.Save(ctx, agentAccount); err != nil {
		t.Fatalf("Save user: %v", err)
	}
	agentPlain, agentTok, err := auth.New("deploy-bot")
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	agentTok.UserID = agentAccount.ID
	agentTok.Kind = auth.KindAgent
	if err := tokens.Save(ctx, agentTok); err != nil {
		t.Fatalf("Save token: %v", err)
	}
	viewerAccount, err := user.New("reader", "pw", user.RoleViewer)
	if err != nil {
		t.Fatalf("user.New: %v", err)
	}
	if err := users.Save(ctx, viewerAccount); err != nil {
		t.Fatalf("Save user: %v", err)
	}
	viewerPlain, viewerTok, err := auth.New("reader-token")
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	viewerTok.UserID = viewerAccount.ID
	if err := tokens.Save(ctx, viewerTok); err != nil {
		t.Fatalf("Save token: %v", err)
	}

	// One run the agent asked for, and one somebody else did.
	ended := time.Now()
	own := &run.Run{
		ID: "run_own", Status: run.StatusSucceeded, CreatedAt: ended, EndedAt: &ended,
		Tool: "bash", Command: "deploy", Actor: "deploy-bot", ActorType: "agent",
	}
	other := &run.Run{
		ID: "run_other", Status: run.StatusSucceeded, CreatedAt: ended, EndedAt: &ended,
		Tool: "bash", Command: "deploy", Actor: "casey", ActorType: "session",
	}
	for _, r := range []*run.Run{own, other} {
		if err := runs.Save(ctx, r); err != nil {
			t.Fatalf("Save run: %v", err)
		}
	}

	handler := New(runs, &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
		WithTokens(tokens), WithUsers(users), WithAudit(audits)).Handler()
	get := func(path, bearer string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec
	}

	// Test 0: The agent reads the evidence for its own run, as JSON, which is what a model can read at
	// all: the HTML dossier is a page for a person.
	rec := get("/v1/runs/run_own/evidence?format=json", agentPlain)
	if rec.Code != http.StatusOK {
		t.Fatalf("agent reading its own evidence = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("content type = %q, want JSON", ct)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode evidence: %v (body %s)", err, rec.Body.String())
	}
	if doc["run"] == nil {
		t.Errorf("evidence JSON = %s, want the run it is about", rec.Body.String())
	}

	// Test 1: Somebody else's run is not the agent's business.
	if rec := get("/v1/runs/run_other/evidence?format=json", agentPlain); rec.Code != http.StatusForbidden {
		t.Errorf("agent reading another actor's evidence = %d, want 403", rec.Code)
	}

	// Test 2: A viewer is refused even for a run they somehow share a name with, because evidence is
	// not reading: it quotes the audit trail.
	if rec := get("/v1/runs/run_own/evidence?format=json", viewerPlain); rec.Code != http.StatusForbidden {
		t.Errorf("viewer reading evidence = %d, want 403", rec.Code)
	}

	// Test 3: The same rule covers the signed receipt, which is the artifact an agent would hand to
	// somebody who does not trust this server.
	if rec := get("/v1/runs/run_own/receipt", agentPlain); rec.Code == http.StatusForbidden {
		t.Error("the agent cannot fetch the receipt for its own run, so it cannot prove what it did")
	}
	if rec := get("/v1/runs/run_other/receipt", agentPlain); rec.Code != http.StatusForbidden {
		t.Errorf("agent fetching another actor's receipt = %d, want 403", rec.Code)
	}
}
