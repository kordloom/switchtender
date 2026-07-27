package secretsource

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestResolveAzure exercises the resolver end to end against mock Key Vault, Entra ID, and Azure
// Instance Metadata Service servers, covering the config token, service principal, and managed
// identity auth paths plus the version, error, and configuration cases.
func TestResolveAzure(t *testing.T) {
	t.Parallel()
	var gotAuth string
	vaultSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if !strings.HasPrefix(gotAuth, "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/secrets/ci", "/secrets/ci/v2":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": "azure-secret-value"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer vaultSrv.Close()

	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "client_credentials" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "sp-token"})
	}))
	defer authSrv.Close()

	imdsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata") != "true" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "mi-token"})
	}))
	defer imdsSrv.Close()

	origEP, origAuth, origIMDS := azureEndpoint, azureAuthEndpoint, azureIMDSEndpoint
	azureEndpoint, azureAuthEndpoint, azureIMDSEndpoint = vaultSrv.URL, authSrv.URL, imdsSrv.URL
	defer func() {
		azureEndpoint, azureAuthEndpoint, azureIMDSEndpoint = origEP, origAuth, origIMDS
	}()

	cfg := func(c azureConfig) string {
		b, _ := json.Marshal(c)
		return string(b)
	}

	// Test 0: A config token is used directly as the bearer.
	got, err := resolveAzure(context.Background(), cfg(azureConfig{Vault: "kv", Secret: "ci", Token: "cfg-token"}))
	if err != nil || got != "azure-secret-value" {
		t.Errorf("config token = %q, %v; want azure-secret-value", got, err)
	}
	if gotAuth != "Bearer cfg-token" {
		t.Errorf("config token auth = %q, want Bearer cfg-token", gotAuth)
	}

	// Test 1: A service principal runs the client-credentials grant, and its token reads the secret.
	got, err = resolveAzure(context.Background(), cfg(azureConfig{
		Vault: "kv", Secret: "ci", TenantID: "t", ClientID: "c", ClientSecret: "s",
	}))
	if err != nil || got != "azure-secret-value" {
		t.Errorf("service principal = %q, %v; want azure-secret-value", got, err)
	}
	if gotAuth != "Bearer sp-token" {
		t.Errorf("service principal auth = %q, want Bearer sp-token", gotAuth)
	}

	// Test 2: With no token or service principal, the metadata service supplies a managed-identity
	// token, which proves the metadata client reaches the endpoint that safeClient would refuse.
	got, err = resolveAzure(context.Background(), cfg(azureConfig{Vault: "kv", Secret: "ci"}))
	if err != nil || got != "azure-secret-value" {
		t.Errorf("managed identity = %q, %v; want azure-secret-value", got, err)
	}
	if gotAuth != "Bearer mi-token" {
		t.Errorf("managed identity auth = %q, want Bearer mi-token", gotAuth)
	}

	// Test 3: A version reads the versioned path.
	if got, err := resolveAzure(context.Background(),
		cfg(azureConfig{Vault: "kv", Secret: "ci", Version: "v2", Token: "cfg-token"})); err != nil || got != "azure-secret-value" {
		t.Errorf("versioned = %q, %v; want azure-secret-value", got, err)
	}

	// Test 4: A missing vault or secret is an error before any request.
	if _, err := resolveAzure(context.Background(), cfg(azureConfig{Secret: "ci", Token: "t"})); !errors.Is(err, ErrResolve) {
		t.Errorf("missing vault error = %v, want ErrResolve", err)
	}
	if _, err := resolveAzure(context.Background(), cfg(azureConfig{Vault: "kv", Token: "t"})); !errors.Is(err, ErrResolve) {
		t.Errorf("missing secret error = %v, want ErrResolve", err)
	}

	// Test 5: A partial service principal is a configuration error, not a fall back to managed identity.
	if _, err := resolveAzure(context.Background(),
		cfg(azureConfig{Vault: "kv", Secret: "ci", TenantID: "t"})); !errors.Is(err, ErrResolve) {
		t.Errorf("partial service principal error = %v, want ErrResolve", err)
	}

	// Test 6: An unknown secret gets a non-200 and errors.
	if _, err := resolveAzure(context.Background(),
		cfg(azureConfig{Vault: "kv", Secret: "missing", Token: "t"})); !errors.Is(err, ErrResolve) {
		t.Errorf("unknown secret error = %v, want ErrResolve", err)
	}

	// Test 7: Invalid config JSON errors.
	if _, err := resolveAzure(context.Background(), "{bad"); !errors.Is(err, ErrResolve) {
		t.Errorf("bad config error = %v, want ErrResolve", err)
	}
}
