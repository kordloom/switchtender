package secretsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// handleVault is a fake Vault for the handle tests: it issues lease db/creds/1 with the lifetime
// it is given and records every lease revoked.
type handleVault struct {
	// srv serves the API.
	srv *httptest.Server
	// mu guards revoked.
	mu sync.Mutex
	// revoked lists the lease ids revoked, in order.
	revoked []string
}

// newHandleVault starts a fake Vault whose reads say lifetime seconds, or no lease at all when
// leaseID is empty.
func newHandleVault(t *testing.T, leaseID string, lifetime int) *handleVault {
	t.Helper()
	v := &handleVault{}
	v.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "hvs.handle" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/db/creds/app":
			_ = json.NewEncoder(w).Encode(map[string]any{"lease_id": leaseID,
				"lease_duration": lifetime, "data": map[string]any{"password": "minted"}})
		case r.Method == http.MethodPut && r.URL.Path == "/v1/sys/leases/revoke":
			var body struct {
				LeaseID string `json:"lease_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			v.mu.Lock()
			v.revoked = append(v.revoked, body.LeaseID)
			v.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(v.srv.Close)
	return v
}

// handleConfig returns a vault_dynamic configuration naming addr.
func handleConfig(addr string) string {
	b, _ := json.Marshal(vaultDynamicConfig{Addr: addr, Path: "db/creds/app", Field: "password",
		Token: "hvs.handle"})
	return string(b)
}

// TestVaultDynamicLeasesCarryAHandleAnotherProcessCanRevoke mints a Vault dynamic secret, takes the
// handle its lease gives, and revokes it with RevokeHandle the way a replica that never held the
// lease does: from the handle and the source's configuration alone.
//
// The handle names the lease and the Vault that issued it and nothing else, the expiry follows the
// lifetime Vault gave, and the token is sent only to the Vault that issued the lease, so a
// credential edited to name another Vault is refused rather than handing its token to the old one.
func TestVaultDynamicLeasesCarryAHandleAnotherProcessCanRevoke(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// LeaseID is the lease Vault issues, empty for a read that issues none.
		LeaseID string
		// Lifetime is the lease_duration Vault answers, in seconds.
		Lifetime int
		// OtherVault revokes with a configuration naming another Vault.
		OtherVault bool
		// WantHandle reports whether the lease gives a handle.
		WantHandle bool
		// WantLifetime is how long after the mint the handle says the secret expires.
		WantLifetime time.Duration
		// WantRevoked is the lease ids the issuing Vault saw revoked.
		WantRevoked []string
		// Want is the error RevokeHandle returns.
		Want error
	}{{ // Test 0: A lease with a lifetime gives a handle that revokes it.
		LeaseID: "db/creds/1", Lifetime: 600, WantHandle: true, WantLifetime: 600 * time.Second,
		WantRevoked: []string{"db/creds/1"},
	}, { // Test 1: A lease whose lifetime Vault did not say is kept for the longest one can last.
		LeaseID: "db/creds/1", WantHandle: true, WantLifetime: UnknownLifetime,
		WantRevoked: []string{"db/creds/1"},
	}, { // Test 2: A credential that now names another Vault is refused, and nothing is sent.
		LeaseID: "db/creds/1", Lifetime: 600, OtherVault: true, WantHandle: true,
		WantLifetime: 600 * time.Second, Want: ErrLeaseHandle,
	}, { // Test 3: A read that issued no lease gives no handle.
		Lifetime: 600,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			v := newHandleVault(t, test.LeaseID, test.Lifetime)
			before := time.Now()
			_, lease, err := mintVaultDynamic(ctx, handleConfig(v.srv.URL))
			if err != nil {
				t.Fatalf("mintVaultDynamic() error = %v", err)
			}
			handle, expires := lease.Handle()
			if diff := cmp.Diff(test.WantHandle, handle != ""); diff != "" {
				t.Fatalf("handle given mismatch (-want +got):\n%s", diff)
			}
			if !test.WantHandle {
				return
			}
			if strings.Contains(handle, "hvs.handle") || strings.Contains(handle, "minted") {
				t.Errorf("the handle carries the token or the secret: %q", handle)
			}
			if lifetime := expires.Sub(before); lifetime < test.WantLifetime ||
				lifetime > test.WantLifetime+time.Minute {
				t.Errorf("the handle expires %v after the mint, want %v", lifetime, test.WantLifetime)
			}
			addr := v.srv.URL
			if test.OtherVault {
				addr = "https://vault.elsewhere.example:8200"
			}
			err = RevokeHandle(ctx, KindVaultDynamic, handle, handleConfig(addr))
			if !errors.Is(err, test.Want) {
				t.Errorf("RevokeHandle() error = %v, want %v", err, test.Want)
			}
			v.mu.Lock()
			got := append([]string(nil), v.revoked...)
			v.mu.Unlock()
			if diff := cmp.Diff(test.WantRevoked, got); diff != "" {
				t.Errorf("revoked leases mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRevokeHandleRefusesWhatItCannotRevoke pins the refusals no retry can fix, each wrapping
// ErrLeaseHandle so a caller drops the handle rather than trying it forever.
func TestRevokeHandleRefusesWhatItCannotRevoke(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Kind   string
		Handle string
		Config string
		Want   error
	}{{ // Test 0: A source kind whose leases give no handle, such as one a plugin registers.
		Kind: "plugin_dynamic", Handle: `{"addr":"https://v","lease_id":"x"}`, Want: ErrLeaseHandle,
	}, { // Test 1: STS credentials cannot be revoked early at all.
		Kind: KindAWSSTS, Handle: "anything", Want: ErrLeaseHandle,
	}, { // Test 2: A handle the engine did not write.
		Kind: KindVaultDynamic, Handle: "not json", Config: handleConfig("https://v"),
		Want: ErrLeaseHandle,
	}, { // Test 3: A configuration that is not the engine's.
		Kind: KindVaultDynamic, Handle: `{"addr":"https://v","lease_id":"x"}`, Config: "not json",
		Want: ErrLeaseHandle,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := RevokeHandle(context.Background(), test.Kind, test.Handle, test.Config)
			if !errors.Is(err, test.Want) {
				t.Errorf("RevokeHandle() error = %v, want %v", err, test.Want)
			}
		})
	}
}
