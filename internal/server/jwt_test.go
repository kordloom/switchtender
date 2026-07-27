package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
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

func TestJWTAuthenticate(t *testing.T) {
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
	defer srv.Close()

	const issuer = "https://issuer.test"
	const audience = "switchtender"
	sign := func(claims jwt.Claims, extra map[string]any) string {
		sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
			(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
		if err != nil {
			t.Fatalf("signer: %v", err)
		}
		raw, err := jwt.Signed(sig).Claims(claims).Claims(extra).Serialize()
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return raw
	}

	a, err := NewJWTAuth(context.Background(), srv.URL, issuer, audience, "sub", "groups",
		user.RoleViewer, map[string]user.Role{"admins": user.RoleAdmin}, user.NewMemStore(), zap.NewNop())
	if err != nil {
		t.Fatalf("NewJWTAuth: %v", err)
	}

	// Test 0: A valid token authenticates and its group maps to a role.
	valid := sign(jwt.Claims{
		Issuer: issuer, Subject: "bob", Audience: jwt.Audience{audience},
		Expiry: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}, map[string]any{"groups": []string{"admins"}})
	u, err := a.Authenticate(context.Background(), valid)
	if err != nil || u.Username != "bob" || u.Role != user.RoleAdmin {
		t.Fatalf("valid = %+v, %v; want bob admin", u, err)
	}

	// Test 1: An expired token is refused.
	expired := sign(jwt.Claims{
		Issuer: issuer, Subject: "bob", Audience: jwt.Audience{audience},
		Expiry: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
	}, nil)
	if _, err := a.Authenticate(context.Background(), expired); err == nil {
		t.Error("expired token accepted, want error")
	}

	// Test 2: A token from a different issuer is refused.
	wrongIss := sign(jwt.Claims{
		Issuer: "https://evil.test", Subject: "bob", Audience: jwt.Audience{audience},
		Expiry: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}, nil)
	if _, err := a.Authenticate(context.Background(), wrongIss); err == nil {
		t.Error("wrong issuer accepted, want error")
	}
}

func TestJWTMutationRecordsAudit(t *testing.T) {
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
	defer srv.Close()

	const issuer = "https://issuer.test"
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	raw, err := jwt.Signed(sig).Claims(jwt.Claims{
		Issuer: issuer, Subject: "svc", Audience: jwt.Audience{"switchtender"},
		Expiry: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}).Claims(map[string]any{"groups": []string{"admins"}}).Serialize()
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	ctx := context.Background()
	jwtAuth, err := NewJWTAuth(ctx, srv.URL, issuer, "switchtender", "sub", "groups",
		user.RoleViewer, map[string]user.Role{"admins": user.RoleAdmin}, user.NewMemStore(), zap.NewNop())
	if err != nil {
		t.Fatalf("NewJWTAuth: %v", err)
	}

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
