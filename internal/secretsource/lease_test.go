package secretsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestRevokeVaultLeaseEndsTheCredentialOrSaysWhyNot pins the revoke path a run's cleanup depends
// on. A dynamic source exists so a database or cloud credential lives only as long as the run, and
// a revoke that silently reports success while the lease survives leaves that credential live until
// its own TTL. Every outcome the Vault API can produce has to be either a real revoke or a reported
// failure the operator can see.
//
//nolint:funlen // Test function.
func TestRevokeVaultLeaseEndsTheCredentialOrSaysWhyNot(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var requests int
	var gotToken, gotLease, gotContentType string
	status := http.StatusNoContent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		gotToken = r.Header.Get("X-Vault-Token")
		gotContentType = r.Header.Get("Content-Type")
		var body struct {
			LeaseID string `json:"lease_id"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		gotLease = body.LeaseID
		code := status
		mu.Unlock()
		w.WriteHeader(code)
	}))
	defer srv.Close()

	// Test 0: A revoke sends the lease id, as JSON, authenticated with the mint's token.
	err := revokeVaultLease(srv.URL, "hvs.mint-token", "database/creds/app/abc")(context.Background())
	if err != nil {
		t.Fatalf("Revoke() = %v, want nil", err)
	}
	mu.Lock()
	if diff := cmp.Diff("database/creds/app/abc", gotLease); diff != "" {
		t.Errorf("revoked lease mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff("hvs.mint-token", gotToken); diff != "" {
		t.Errorf("revoke token mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff("application/json", gotContentType); diff != "" {
		t.Errorf("revoke content type mismatch (-want +got):\n%s", diff)
	}
	before := requests
	mu.Unlock()

	// Test 1: An empty lease id is a no-op that makes no request, as its doc comment promises.
	if err := revokeVaultLease(srv.URL, "t", "")(context.Background()); err != nil {
		t.Errorf("empty lease Revoke() = %v, want nil", err)
	}
	mu.Lock()
	if requests != before {
		t.Errorf("an empty lease id still sent %d requests, want none", requests-before)
	}
	mu.Unlock()

	// Test 2: A plain 200, as some Vault versions answer with, is also a success.
	mu.Lock()
	status = http.StatusOK
	mu.Unlock()
	if err := revokeVaultLease(srv.URL, "t", "l")(context.Background()); err != nil {
		t.Errorf("200 Revoke() = %v, want nil", err)
	}

	// Test 3: Any other status is a reported failure, so the operator learns the credential lives on.
	for testNum, code := range []int{
		http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusAccepted,
	} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			mu.Lock()
			status = code
			mu.Unlock()
			err := revokeVaultLease(srv.URL, "t", "l")(context.Background())
			if !errors.Is(err, ErrResolve) {
				t.Errorf("status %d Revoke() = %v, want ErrResolve", code, err)
			}
		})
	}

	// Test 4: A Vault that cannot be reached is a reported failure, not a silent success.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	if err := revokeVaultLease(deadURL, "t", "l")(context.Background()); !errors.Is(err, ErrResolve) {
		t.Errorf("unreachable Vault Revoke() = %v, want ErrResolve", err)
	}

	// Test 5: A canceled context is a reported failure.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := revokeVaultLease(srv.URL, "t", "l")(canceled); err == nil {
		t.Error("Revoke() with a canceled context = nil, want an error")
	}
}

// TestRevokeVaultLeaseNeverLeaksTheMintTokenIntoItsError pins that a failed revoke stays as quiet
// as a failed resolve. Cleanup errors are logged and attached to the run, and the token that minted
// the credential is the same Vault token the source authenticates with.
func TestRevokeVaultLeaseNeverLeaksTheMintTokenIntoItsError(t *testing.T) {
	t.Parallel()
	const token = "hvs.REVOKE-TOKEN-MUST-NOT-LEAK"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["permission denied for ` + token + `"]}`))
	}))
	defer srv.Close()

	err := revokeVaultLease(srv.URL, token, "database/creds/app/abc")(context.Background())
	if !errors.Is(err, ErrResolve) {
		t.Fatalf("Revoke() = %v, want ErrResolve", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("the revoke error carries the mint token, which cleanup logs:\n%v", err)
	}
}

// TestMintVaultDynamicLeaseRevokesTheLeaseItMinted checks the wiring between the mint and the lease
// it hands back. A lease built with the wrong address, the wrong token, or a lease id from
// somewhere else looks correct to every test of revokeVaultLease on its own, and only fails in
// production by leaving a live credential behind.
func TestMintVaultDynamicLeaseRevokesTheLeaseItMinted(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var revokedLease, revokeToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			mu.Lock()
			revokeToken = r.Header.Get("X-Vault-Token")
			var body struct {
				LeaseID string `json:"lease_id"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			revokedLease = body.LeaseID
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(
			`{"lease_id":"aws/creds/deploy/xyz789","data":{"secret_key":"minted-key"}}`))
	}))
	defer srv.Close()

	cfg := `{"addr":"` + srv.URL + `/","path":"/aws/creds/deploy","field":"secret_key",` +
		`"token":"hvs.mint-token"}`
	value, lease, err := mintVaultDynamic(context.Background(), cfg)
	if err != nil {
		t.Fatalf("mintVaultDynamic: %v", err)
	}
	if diff := cmp.Diff("minted-key", value); diff != "" {
		t.Errorf("minted value mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(KindVaultDynamic, lease.Kind()); diff != "" {
		t.Errorf("lease kind mismatch (-want +got):\n%s", diff)
	}
	if err := lease.Revoke(context.Background()); err != nil {
		t.Fatalf("Revoke() = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if diff := cmp.Diff("aws/creds/deploy/xyz789", revokedLease); diff != "" {
		t.Errorf("the lease revoked is not the one the mint returned (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff("hvs.mint-token", revokeToken); diff != "" {
		t.Errorf("the revoke used a different token from the mint (-want +got):\n%s", diff)
	}
}

// TestResolversAreSafeForConcurrentUse pins that a fleet-wide run, which opens every credential it
// needs at once, does not race in this package. The resolver and minter tables are written only at
// startup and read on every resolve, and one HTTP client is shared by every resolver, so a lock
// taken in the wrong place or a table written late would show up here under the race detector
// rather than as a crashed server mid-run.
func TestResolversAreSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte(`{"lease_id":"l1","data":{"data":{"token":"kv2"},"password":"pw"}}`))
	}))
	defer srv.Close()

	cases := []struct {
		// Kind is the source to resolve.
		Kind string
		// Config is that source's config.
		Config string
		// WantValue is the value every goroutine must see.
		WantValue string
	}{
		{Kind: KindLocal, Config: "literal-value", WantValue: "literal-value"},
		{Kind: KindCommand, Config: "printf concurrent", WantValue: "concurrent"},
		{
			Kind:      KindVault,
			Config:    `{"addr":"` + srv.URL + `","path":"secret/data/ci","field":"token","token":"t"}`,
			WantValue: "kv2",
		},
		{
			Kind: KindVaultDynamic,
			Config: `{"addr":"` + srv.URL + `","path":"database/creds/app","field":"password",` +
				`"token":"t"}`,
			WantValue: "pw",
		},
		{
			Kind:      KindConjur,
			Config:    `{"url":"` + srv.URL + `","account":"prod","variable":"v","token":"t"}`,
			WantValue: `{"lease_id":"l1","data":{"data":{"token":"kv2"},"password":"pw"}}`,
		},
	}

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers*len(cases)*2)
	for range workers {
		for _, c := range cases {
			wg.Add(1)
			go func(kind, config, want string) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				value, lease, err := ResolveLeased(ctx, kind, config)
				if err != nil {
					errs <- fmt.Errorf("%s: %w", kind, err)
					return
				}
				if value != want {
					errs <- fmt.Errorf("%s resolved to %q, want %q", kind, value, want)
				}
				if err := lease.Revoke(ctx); err != nil {
					errs <- fmt.Errorf("%s revoke: %w", kind, err)
				}
			}(c.Kind, c.Config, c.WantValue)

			// Read-only registry queries run alongside the resolves, since the API serves them while runs
			// are in flight.
			wg.Add(1)
			go func(kind string) {
				defer wg.Done()
				if !ValidKind(kind) || !Registered(kind) || len(Kinds()) == 0 {
					errs <- fmt.Errorf("%s: the registry disagreed with itself under load", kind)
				}
			}(c.Kind)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if hits.Load() == 0 {
		t.Error("no resolver reached the mock store, so the concurrency was not exercised")
	}
}
