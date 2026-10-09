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
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/user"
)

// TestIdentityProviderFetchesAreGuarded pins that every address an identity provider is reached
// at dials through the guard every other configured address uses: the SAML metadata document, the
// JWT key set, OIDC discovery, and the OIDC code exchange that carries the client secret. Each is
// operator configuration the server follows on its own network, and a default client follows it
// anywhere: through the ambient proxy, and to any address at all.
//
// The server stands behind the unspecified address, which the guard refuses and which a plain dial
// turns into this host, so a client that reaches it is a client with no guard. Every other refused
// address has nothing listening, which would make the two clients indistinguishable. Only requests
// addressed to the unspecified address are counted, so the code exchange case can discover the
// provider on loopback, which the guard allows, and still be held to a token endpoint it refuses.
func TestIdentityProviderFetchesAreGuarded(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
		{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"},
	}}
	var hits atomic.Int32
	var idpURL, refused string
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Host, "0.0.0.0:") {
			hits.Add(1)
		}
		switch r.URL.Path {
		case "/jwks":
			_ = json.NewEncoder(w).Encode(jwks)
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                 idpURL,
				"authorization_endpoint": idpURL + "/authorize",
				"token_endpoint":         refused + "/token",
				"jwks_uri":               idpURL + "/jwks",
			})
		case "/token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "at", "token_type": "bearer",
			})
		default:
			_, _ = w.Write([]byte(testIDPMetadata))
		}
	}))
	t.Cleanup(idp.Close)
	idpURL = idp.URL
	refused = strings.Replace(idp.URL, "127.0.0.1", "0.0.0.0", 1)
	certFile, keyFile := writeTestKeypair(t)
	const redirectURL = "https://switchtender.example.com/auth/oidc/callback"

	const issuer = "https://issuer.test"
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatalf("NewSigner() error = %v", err)
	}
	token, err := jwt.Signed(signer).Claims(jwt.Claims{
		Issuer: issuer, Subject: "bob", Expiry: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}).Serialize()
	if err != nil {
		t.Fatalf("Serialize() error = %v", err)
	}

	tests := []struct {
		// Name labels the case in failure output.
		Name string
		// Fetch makes the request under test and returns its error, nil when it succeeded.
		Fetch func(t *testing.T) error
	}{{ // Test 0: The SAML metadata fetch at startup is refused at the dial.
		Name: "saml metadata",
		Fetch: func(t *testing.T) error {
			t.Helper()
			_, err := NewSAMLAuth(context.Background(), refused, "https://switchtender.example.com",
				certFile, keyFile, "", "groups", user.RoleViewer, nil,
				user.NewMemStore(), auth.NewMemStore(), zap.NewNop())
			return err
		},
	}, { // Test 1: The key set fetch a verification triggers is refused at the dial.
		Name: "jwks",
		Fetch: func(t *testing.T) error {
			t.Helper()
			a, err := NewJWTAuth(context.Background(), refused+"/jwks", issuer, "", "sub", "",
				user.RoleViewer, nil, user.NewMemStore(), zap.NewNop())
			if err != nil {
				t.Fatalf("NewJWTAuth() error = %v", err)
			}
			_, err = a.Authenticate(context.Background(), token)
			return err
		},
	}, { // Test 2: The OIDC discovery --oidc-issuer names at startup is refused at the dial.
		Name: "oidc issuer",
		Fetch: func(t *testing.T) error {
			t.Helper()
			_, err := NewOIDCAuth(context.Background(), refused, "test-client", "test-secret",
				redirectURL, user.RoleViewer, user.NewMemStore(), auth.NewMemStore(), zap.NewNop())
			return err
		},
	}, { // Test 3: The OIDC code exchange a sign-in callback makes is refused at the dial.
		Name: "oidc code exchange",
		Fetch: func(t *testing.T) error {
			t.Helper()
			o, err := NewOIDCAuth(context.Background(), idpURL, "test-client", "test-secret",
				redirectURL, user.RoleViewer, user.NewMemStore(), auth.NewMemStore(), zap.NewNop())
			if err != nil {
				t.Fatalf("NewOIDCAuth() error = %v", err)
			}
			rec := httptest.NewRecorder()
			o.callback(rec, signedHandshake(t, o, "s1", "n1", "state=s1&code=c1"))
			loc := rec.Header().Get("Location")
			if strings.HasPrefix(loc, "/ui/#") {
				return nil
			}
			return fmt.Errorf("callback redirected to %s", loc)
		},
	}}
	// The cases share the arrival counter, so they run in order.
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			before := hits.Load()
			if err := test.Fetch(t); err == nil {
				t.Errorf("%s: fetch through a refused address succeeded", test.Name)
			}
			if n := hits.Load() - before; n != 0 {
				t.Errorf("%s: the identity provider was reached %d times through a refused "+
					"address, so the fetch is not going through the dial guard", test.Name, n)
			}
		})
	}
}
