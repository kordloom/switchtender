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

// fakeJWT stands in for the paid JWT directory: it accepts the tokens it was given and reports
// which of them an issuer marked as an agent's. The tokens carry two dots so the gate routes them
// to JWT validation.
type fakeJWT struct {
	// users maps a raw token to the account it signs in.
	users map[string]*user.User
	// agents marks the tokens that carry the agent claim.
	agents map[string]bool
}

// Authenticate returns the account for raw or refuses.
func (f fakeJWT) Authenticate(_ context.Context, raw string) (*user.User, error) {
	u, ok := f.users[raw]
	if !ok {
		return nil, ErrJWTAuth
	}
	return u, nil
}

// AuthenticateActor is Authenticate that also reports the agent mark.
func (f fakeJWT) AuthenticateActor(ctx context.Context, raw string) (*user.User, bool, error) {
	u, err := f.Authenticate(ctx, raw)
	return u, f.agents[raw], err
}

// TestJWTMutationRecordsAudit checks that a mutation made with a bearer JWT is recorded in the
// audit chain under the token's subject, through the directory interface the paid module fills.
func TestJWTMutationRecordsAudit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const raw = "h.p.s"
	jwtAuth := fakeJWT{users: map[string]*user.User{raw: {Username: "svc", Role: user.RoleAdmin}}}

	// A token must exist so the gate enforces auth; the JWT bearer is the path under test.
	tokens := auth.NewMemStore()
	_, tok, err := auth.New("bootstrap")
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	if err := tokens.Save(ctx, tok); err != nil {
		t.Fatalf("tokens.Save: %v", err)
	}

	audits := audit.NewMemStore()
	handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_j"}}, zap.NewNop(),
		WithTokens(tokens), WithJWT(jwtAuth), WithAudit(audits)).Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(`{"playbook":"p.yml"}`))
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /runs with JWT status = %d, want 202", rec.Code)
	}

	// The audit append runs in a goroutine, so poll briefly for the entry the JWT actor produced.
	var entries []*audit.Entry
	for range 50 {
		if entries, _ = audits.List(ctx, 10); len(entries) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want 1 (JWT mutation was not recorded)", len(entries))
	}
	if got := entries[0]; got.Actor != "svc" || got.Method != http.MethodPost || got.Path != "/v1/runs" {
		t.Errorf("entry = {actor:%q method:%q path:%q}, want {svc POST /runs}",
			got.Actor, got.Method, got.Path)
	}
}

// TestJWTRefusedWhenNoDirectory checks that a bearer that looks like a JWT is refused when no JWT
// directory is wired, which is the Community build's shape.
func TestJWTRefusedWhenNoDirectory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tokens := auth.NewMemStore()
	_, tok, err := auth.New("bootstrap")
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	if err := tokens.Save(ctx, tok); err != nil {
		t.Fatalf("tokens.Save: %v", err)
	}
	handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_j"}}, zap.NewNop(),
		WithTokens(tokens)).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(`{"playbook":"p.yml"}`))
	req.Header.Set("Authorization", "Bearer h.p.s")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /runs with a JWT and no directory status = %d, want 401", rec.Code)
	}
}
