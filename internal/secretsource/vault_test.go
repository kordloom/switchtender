package secretsource

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveVault(t *testing.T) {
	// Serial: t.Setenv is incompatible with t.Parallel.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "hvs.test" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/v1/secret/data/ci": // KV v2 nests under data.data.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"data": map[string]any{"token": "kv2-secret"}},
			})
		case "/v1/secret/ci": // KV v1 sits under data.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"token": "kv1-secret"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := func(path, field, token string) string {
		b, _ := json.Marshal(vaultConfig{Addr: srv.URL, Path: path, Field: field, Token: token})
		return string(b)
	}

	// Test 0: KV v2 resolves from the nested data.data.
	if got, err := resolveVault(context.Background(), cfg("secret/data/ci", "token", "hvs.test")); err != nil || got != "kv2-secret" {
		t.Errorf("KV v2 = %q, %v; want kv2-secret", got, err)
	}
	// Test 1: KV v1 resolves from the flat data.
	if got, err := resolveVault(context.Background(), cfg("secret/ci", "token", "hvs.test")); err != nil || got != "kv1-secret" {
		t.Errorf("KV v1 = %q, %v; want kv1-secret", got, err)
	}
	// Test 2: A missing field is an error.
	if _, err := resolveVault(context.Background(), cfg("secret/data/ci", "nope", "hvs.test")); !errors.Is(err, ErrResolve) {
		t.Errorf("missing field error = %v, want ErrResolve", err)
	}
	// Test 3: A bad token gets a non-200 and errors.
	if _, err := resolveVault(context.Background(), cfg("secret/data/ci", "token", "wrong")); !errors.Is(err, ErrResolve) {
		t.Errorf("bad token error = %v, want ErrResolve", err)
	}
	// Test 4: Invalid config JSON errors.
	if _, err := resolveVault(context.Background(), "{not json"); !errors.Is(err, ErrResolve) {
		t.Errorf("bad config error = %v, want ErrResolve", err)
	}
	// Test 5: No config token and no VAULT_TOKEN errors before any request.
	t.Setenv("VAULT_TOKEN", "")
	if _, err := resolveVault(context.Background(), cfg("secret/data/ci", "token", "")); !errors.Is(err, ErrResolve) {
		t.Errorf("missing token error = %v, want ErrResolve", err)
	}
	// Test 6: VAULT_TOKEN supplies the token when the config omits it and the addr is the pinned
	// VAULT_ADDR, the server's own Vault.
	t.Setenv("VAULT_ADDR", srv.URL)
	t.Setenv("VAULT_TOKEN", "hvs.test")
	if got, err := resolveVault(context.Background(), cfg("secret/data/ci", "token", "")); err != nil || got != "kv2-secret" {
		t.Errorf("VAULT_TOKEN fallback = %q, %v; want kv2-secret", got, err)
	}
	// Test 7: A config addr that is not the pinned VAULT_ADDR does not receive the env VAULT_TOKEN.
	// Were it sent, the mock server would accept it and return the secret; the read must fail instead,
	// so the server's Vault token cannot reach an attacker-chosen host.
	t.Setenv("VAULT_ADDR", "https://vault.internal.example:8200")
	t.Setenv("VAULT_TOKEN", "hvs.test")
	if _, err := resolveVault(context.Background(), cfg("secret/data/ci", "token", "")); !errors.Is(err, ErrResolve) {
		t.Errorf("unpinned addr fallback error = %v, want ErrResolve", err)
	}
}
