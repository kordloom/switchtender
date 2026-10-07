package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestJWTAuthenticateActorReadsTheAgentClaim holds a federated token to the agent claim the issuer
// signs: only the configured claim set to agent marks its holder as an AI agent.
func TestJWTAuthenticateActorReadsTheAgentClaim(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
		{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	t.Cleanup(srv.Close)

	const issuer = "https://issuer.test"
	const audience = "switchtender"
	sign := func(extra map[string]any) string {
		sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
			(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
		if err != nil {
			t.Fatalf("signer: %v", err)
		}
		raw, err := jwt.Signed(sig).Claims(jwt.Claims{
			Issuer: issuer, Subject: "svc-deploy", Audience: jwt.Audience{audience},
			Expiry: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		}).Claims(extra).Serialize()
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return raw
	}

	tests := []struct {
		Name      string
		Claim     string
		Extra     map[string]any
		WantAgent bool
	}{{ // Test 0: The default claim set to agent marks an agent.
		Name:      "default claim is agent",
		Extra:     map[string]any{DefaultJWTAgentClaim: "agent"},
		WantAgent: true,
	}, { // Test 1: A token without the claim is a person's.
		Name:      "no claim",
		Extra:     map[string]any{},
		WantAgent: false,
	}, { // Test 2: Any other value does not mark an agent.
		Name:      "other value",
		Extra:     map[string]any{DefaultJWTAgentClaim: "person"},
		WantAgent: false,
	}, { // Test 3: A configured claim name is read in place of the default.
		Name:      "configured claim",
		Claim:     "kind",
		Extra:     map[string]any{"kind": "agent"},
		WantAgent: true,
	}, { // Test 4: The default name is ignored once another is configured.
		Name:      "default ignored when configured",
		Claim:     "kind",
		Extra:     map[string]any{DefaultJWTAgentClaim: "agent"},
		WantAgent: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			a, err := NewJWTAuth(context.Background(), srv.URL, issuer, audience, "sub", "groups",
				user.RoleViewer, nil, user.NewMemStore(), zap.NewNop())
			if err != nil {
				t.Fatalf("NewJWTAuth: %v", err)
			}
			a = a.WithAgentClaim(test.Claim)
			u, agent, err := a.AuthenticateActor(context.Background(), sign(test.Extra))
			if err != nil || u == nil {
				t.Fatalf("AuthenticateActor = %+v, %v; want a provisioned account", u, err)
			}
			if agent != test.WantAgent {
				t.Errorf("agent = %v, want %v", agent, test.WantAgent)
			}
		})
	}
}

// TestFederatedAgentTokenIsHeldToTheAgentCeiling sends a federated token marked as an agent's through
// the server. The agent is refused an admin-only route that its group would otherwise reach, and its
// run is recorded as an agent's rather than a person's.
func TestFederatedAgentTokenIsHeldToTheAgentCeiling(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
		{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	t.Cleanup(srv.Close)

	const issuer = "https://issuer.test"
	sign := func(extra map[string]any) string {
		sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
			(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
		if err != nil {
			t.Fatalf("signer: %v", err)
		}
		raw, err := jwt.Signed(sig).Claims(jwt.Claims{
			Issuer: issuer, Subject: "svc-deploy", Audience: jwt.Audience{"switchtender"},
			Expiry: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		}).Claims(map[string]any{"groups": []string{"admins"}}).Claims(extra).Serialize()
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return raw
	}

	users := user.NewMemStore()
	jwtAuth, err := NewJWTAuth(context.Background(), srv.URL, issuer, "switchtender", "sub", "groups",
		user.RoleViewer, map[string]user.Role{"admins": user.RoleAdmin}, users, zap.NewNop())
	if err != nil {
		t.Fatalf("NewJWTAuth: %v", err)
	}
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
	agentReq.Header.Set("Authorization", "Bearer "+sign(map[string]any{DefaultJWTAgentClaim: "agent"}))
	agentRec := httptest.NewRecorder()
	handler.ServeHTTP(agentRec, agentReq)
	if agentRec.Code != http.StatusForbidden {
		t.Fatalf("agent POST /v1/users status = %d, want 403", agentRec.Code)
	}

	// Test 1: The same group, without the agent claim, reaches that route.
	personReq := httptest.NewRequest(http.MethodPost, "/v1/users",
		strings.NewReader(`{"username":"new-admin","password":"correct-horse-battery","role":"admin"}`))
	personReq.Header.Set("Authorization", "Bearer "+sign(nil))
	personRec := httptest.NewRecorder()
	handler.ServeHTTP(personRec, personReq)
	if personRec.Code != http.StatusCreated {
		t.Fatalf("person POST /v1/users status = %d, want 201: %s", personRec.Code, personRec.Body.String())
	}

	// Test 2: The agent's run is recorded with the agent actor type.
	runReq := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(`{"playbook":"p.yml"}`))
	runReq.Header.Set("Authorization", "Bearer "+sign(map[string]any{DefaultJWTAgentClaim: "agent"}))
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
