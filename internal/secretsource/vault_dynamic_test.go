package secretsource

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestMintVaultDynamic(t *testing.T) {
	var mu sync.Mutex
	revoked := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "hvs.test" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/database/creds/app":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"lease_id": "database/creds/app/abc123",
				"data":     map[string]any{"username": "v-app-x", "password": "s3cr3t-pw"},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/v1/sys/leases/revoke":
			var body struct {
				LeaseID string `json:"lease_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			revoked = body.LeaseID
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := func(path, field, token string) string {
		b, _ := json.Marshal(vaultDynamicConfig{Addr: srv.URL, Path: path, Field: field, Token: token})
		return string(b)
	}

	// Test 0: A read mints the requested field and returns a lease naming the engine.
	value, lease, err := mintVaultDynamic(context.Background(), cfg("database/creds/app", "password", "hvs.test"))
	if err != nil || value != "s3cr3t-pw" {
		t.Fatalf("mint = %q, %v; want s3cr3t-pw", value, err)
	}
	if lease.Kind() != KindVaultDynamic {
		t.Errorf("lease kind = %q, want %q", lease.Kind(), KindVaultDynamic)
	}
	// Test 1: Revoking the lease ends the Vault lease by its id.
	if err := lease.Revoke(context.Background()); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}
	mu.Lock()
	got := revoked
	mu.Unlock()
	if got != "database/creds/app/abc123" {
		t.Errorf("revoked lease = %q, want database/creds/app/abc123", got)
	}
	// Test 2: A missing field is an error.
	if _, _, err := mintVaultDynamic(context.Background(), cfg("database/creds/app", "nope", "hvs.test")); !errors.Is(err, ErrResolve) {
		t.Errorf("missing field error = %v, want ErrResolve", err)
	}
	// Test 3: A bad token gets a non-200 and errors.
	if _, _, err := mintVaultDynamic(context.Background(), cfg("database/creds/app", "password", "wrong")); !errors.Is(err, ErrResolve) {
		t.Errorf("bad token error = %v, want ErrResolve", err)
	}
	// Test 4: Invalid config JSON errors.
	if _, _, err := mintVaultDynamic(context.Background(), "{not json"); !errors.Is(err, ErrResolve) {
		t.Errorf("bad config error = %v, want ErrResolve", err)
	}
	// Test 5: A missing token errors before any request.
	t.Setenv("VAULT_TOKEN", "")
	if _, _, err := mintVaultDynamic(context.Background(), cfg("database/creds/app", "password", "")); !errors.Is(err, ErrResolve) {
		t.Errorf("missing token error = %v, want ErrResolve", err)
	}
	// Test 6: VAULT_TOKEN mints the secret when the config omits the token and the addr is the pinned
	// VAULT_ADDR, the server's own Vault.
	t.Setenv("VAULT_ADDR", srv.URL)
	t.Setenv("VAULT_TOKEN", "hvs.test")
	if value, _, err := mintVaultDynamic(context.Background(), cfg("database/creds/app", "password", "")); err != nil || value != "s3cr3t-pw" {
		t.Errorf("VAULT_TOKEN fallback = %q, %v; want s3cr3t-pw", value, err)
	}
	// Test 7: A config addr that is not the pinned VAULT_ADDR does not receive the env VAULT_TOKEN, so
	// the server's Vault token cannot reach an attacker-chosen host.
	t.Setenv("VAULT_ADDR", "https://vault.internal.example:8200")
	t.Setenv("VAULT_TOKEN", "hvs.test")
	if _, _, err := mintVaultDynamic(context.Background(), cfg("database/creds/app", "password", "")); !errors.Is(err, ErrResolve) {
		t.Errorf("unpinned addr fallback error = %v, want ErrResolve", err)
	}
}

func TestResolveLeased(t *testing.T) {
	t.Parallel()
	// Test 0: A local source returns its config with no lease.
	v, lease, err := ResolveLeased(context.Background(), KindLocal, "plainvalue")
	if err != nil || v != "plainvalue" || lease != nil {
		t.Errorf("local = %q, lease %v, %v; want plainvalue and no lease", v, lease, err)
	}
	// Test 1: A plain resolver returns its value with no lease.
	v, lease, err = ResolveLeased(context.Background(), KindCommand, "printf hi")
	if err != nil || v != "hi" || lease != nil {
		t.Errorf("command = %q, lease %v, %v; want hi and no lease", v, lease, err)
	}
	// Test 2: A nil lease revokes as a no-op.
	if err := lease.Revoke(context.Background()); err != nil {
		t.Errorf("nil lease Revoke() = %v, want nil", err)
	}
	// Test 3: An unknown kind is an error.
	if _, _, err := ResolveLeased(context.Background(), "nope", "x"); !errors.Is(err, ErrResolve) {
		t.Errorf("unknown kind error = %v, want ErrResolve", err)
	}
}
