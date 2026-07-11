package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/user"
)

func TestJWTAuthenticate(t *testing.T) {
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
	const audience = "yardmaster"
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
