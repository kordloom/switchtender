package secretsource

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestResolveConjur exercises the resolver end to end against a mock CyberArk Conjur server, covering
// the API-key exchange, the pre-fetched token path, the version path, and the error cases.
func TestResolveConjur(t *testing.T) {
	t.Parallel()

	const apiKey = "conjur-api-key"
	const tokenBody = "conjur-access-token"
	wantExchangedAuth := `Token token="` + base64.StdEncoding.EncodeToString([]byte(tokenBody)) + `"`

	var gotAuth, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/authn/prod/app/authenticate":
			body, _ := io.ReadAll(r.Body)
			if string(body) != apiKey {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(tokenBody))
		case r.Method == http.MethodGet && r.URL.Path == "/secrets/prod/variable/db/password":
			gotAuth = r.Header.Get("Authorization")
			gotVersion = r.URL.Query().Get("version")
			_, _ = w.Write([]byte("s3cr3t"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := func(c conjurConfig) string {
		c.URL = srv.URL
		b, _ := json.Marshal(c)
		return string(b)
	}

	// Test 0: An API key is exchanged for a token, which then reads the variable.
	got, err := resolveConjur(context.Background(), cfg(conjurConfig{
		Account: "prod", Login: "app", APIKey: apiKey, Variable: "db/password",
	}))
	if err != nil || got != "s3cr3t" {
		t.Errorf("api key = %q, %v; want s3cr3t", got, err)
	}
	if gotAuth != wantExchangedAuth {
		t.Errorf("exchanged auth = %q, want %q", gotAuth, wantExchangedAuth)
	}

	// Test 1: A config token is used directly, with no authn exchange.
	got, err = resolveConjur(context.Background(), cfg(conjurConfig{
		Account: "prod", Variable: "db/password", Token: "cfg-token",
	}))
	if err != nil || got != "s3cr3t" {
		t.Errorf("config token = %q, %v; want s3cr3t", got, err)
	}
	if gotAuth != `Token token="cfg-token"` {
		t.Errorf("config token auth = %q, want Token token=\"cfg-token\"", gotAuth)
	}

	// Test 2: A version is sent as a query parameter.
	if _, err := resolveConjur(context.Background(), cfg(conjurConfig{
		Account: "prod", Variable: "db/password", Token: "cfg-token", Version: "2",
	})); err != nil || gotVersion != "2" {
		t.Errorf("version = %q, err %v; want 2", gotVersion, err)
	}

	// Test 3: A missing url, account, or variable is an error before any request.
	if _, err := resolveConjur(context.Background(),
		`{"account":"prod","variable":"db/password","token":"t"}`); !errors.Is(err, ErrResolve) {
		t.Errorf("missing url error = %v, want ErrResolve", err)
	}
	if _, err := resolveConjur(context.Background(), cfg(conjurConfig{
		Account: "prod", Token: "t",
	})); !errors.Is(err, ErrResolve) {
		t.Errorf("missing variable error = %v, want ErrResolve", err)
	}

	// Test 4: Neither a token nor a login and API key is an error.
	if _, err := resolveConjur(context.Background(), cfg(conjurConfig{
		Account: "prod", Variable: "db/password",
	})); !errors.Is(err, ErrResolve) {
		t.Errorf("missing auth error = %v, want ErrResolve", err)
	}

	// Test 5: An unknown variable gets a non-200 and errors.
	if _, err := resolveConjur(context.Background(), cfg(conjurConfig{
		Account: "prod", Variable: "db/missing", Token: "cfg-token",
	})); !errors.Is(err, ErrResolve) {
		t.Errorf("unknown variable error = %v, want ErrResolve", err)
	}

	// Test 6: Invalid config JSON errors.
	if _, err := resolveConjur(context.Background(), "{bad"); !errors.Is(err, ErrResolve) {
		t.Errorf("bad config error = %v, want ErrResolve", err)
	}
}
