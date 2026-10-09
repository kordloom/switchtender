package secretsource

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// wirePath records the escaped path and query of the last request a mock store received, which is
// what the store would route on. The resolver under test runs on this goroutine, so the lock only
// keeps the race detector honest about the handler goroutine.
type wirePath struct {
	// mu guards path and query.
	mu sync.Mutex
	// path is the request path exactly as it crossed the wire.
	path string
	// query is the raw query string, empty when the request carried none.
	query string
}

// record stores the request's wire path and query.
func (w *wirePath) record(r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.path, w.query = r.URL.EscapedPath(), r.URL.RawQuery
}

// seen returns the recorded path and query.
func (w *wirePath) seen() (string, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.path, w.query
}

// TestGSMRequestKeepsEachNameInItsOwnPathSegment pins that a project, secret, or version is sent as
// one escaped path segment. Unescaped, a secret name with a slash walks into a different resource
// on the host, a query mark turns the rest of the path into a query string, and the store answers
// for whatever those characters splice together rather than for the secret the config names.
//
// It does not run in parallel: it points the package-level gsmEndpoint at its own mock, and
// TestResolveGSM swaps the same variable, so running the two at once makes each read the other's
// server.
func TestGSMRequestKeepsEachNameInItsOwnPathSegment(t *testing.T) {
	var got wirePath
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
		payload := base64.StdEncoding.EncodeToString([]byte("value"))
		_ = json.NewEncoder(w).Encode(map[string]any{"payload": map[string]any{"data": payload}})
	}))
	defer srv.Close()
	origEP := gsmEndpoint
	gsmEndpoint = srv.URL
	defer func() { gsmEndpoint = origEP }()

	tests := []struct {
		// Config is the secret reference the resolver reads.
		Config gsmConfig
		// WantPath is the escaped path the mock Secret Manager must receive.
		WantPath string
	}{{ // Test 0: A secret name with a slash and a space stays one segment.
		Config:   gsmConfig{Project: "proj", Secret: "a/b c", Token: "t"},
		WantPath: "/v1/projects/proj/secrets/a%2Fb%20c/versions/latest:access",
	}, { // Test 1: A query mark in the secret name does not start a query string.
		Config:   gsmConfig{Project: "proj", Secret: "s?x=1", Token: "t"},
		WantPath: "/v1/projects/proj/secrets/s%3Fx=1/versions/latest:access",
	}, { // Test 2: A version with dot segments cannot climb out of the versions collection.
		Config:   gsmConfig{Project: "proj", Secret: "ci", Version: "1/../2", Token: "t"},
		WantPath: "/v1/projects/proj/secrets/ci/versions/1%2F..%2F2:access",
	}, { // Test 3: A project id with a slash stays one segment.
		Config:   gsmConfig{Project: "p/q", Secret: "ci", Token: "t"},
		WantPath: "/v1/projects/p%2Fq/secrets/ci/versions/latest:access",
	}, { // Test 4: A bad percent escape in the secret name is sent as that literal name.
		Config:   gsmConfig{Project: "proj", Secret: "ci%zz", Token: "t"},
		WantPath: "/v1/projects/proj/secrets/ci%25zz/versions/latest:access",
	}, { // Test 5: A well-formed name is sent byte for byte.
		Config:   gsmConfig{Project: "my-proj", Secret: "ci_token-1", Version: "3", Token: "t"},
		WantPath: "/v1/projects/my-proj/secrets/ci_token-1/versions/3:access",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			raw, err := json.Marshal(test.Config)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			if _, err := resolveGSM(context.Background(), string(raw)); err != nil {
				t.Fatalf("resolveGSM() error = %v", err)
			}
			path, query := got.seen()
			if diff := cmp.Diff(test.WantPath, path, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("wire path mismatch (-want +got):\n%s", diff)
			}
			if query != "" {
				t.Errorf("wire query = %q, want none: a name must not become a query string", query)
			}
		})
	}
}

// TestAzureRequestKeepsSecretAndVersionInTheirOwnSegments pins the same rule for Key Vault, whose
// secret name and version are the two config pieces that land in the request path.
//
// It does not run in parallel: it points the package-level azureEndpoint at its own mock, and
// TestResolveAzure swaps the same variable.
func TestAzureRequestKeepsSecretAndVersionInTheirOwnSegments(t *testing.T) {
	var got wirePath
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
		_ = json.NewEncoder(w).Encode(map[string]any{"value": "value"})
	}))
	defer srv.Close()
	origEP := azureEndpoint
	azureEndpoint = srv.URL
	defer func() { azureEndpoint = origEP }()

	tests := []struct {
		// Config is the secret reference the resolver reads.
		Config azureConfig
		// WantPath is the escaped path the mock Key Vault must receive.
		WantPath string
	}{{ // Test 0: A secret name with a slash and a space stays one segment.
		Config:   azureConfig{Vault: "vault1", Secret: "a/b c", Token: "t"},
		WantPath: "/secrets/a%2Fb%20c",
	}, { // Test 1: A version with a slash stays one segment under the secret.
		Config:   azureConfig{Vault: "vault1", Secret: "s", Version: "v/1", Token: "t"},
		WantPath: "/secrets/s/v%2F1",
	}, { // Test 2: A bad percent escape in the secret name is sent as that literal name.
		Config:   azureConfig{Vault: "vault1", Secret: "ci%zz", Token: "t"},
		WantPath: "/secrets/ci%25zz",
	}, { // Test 3: A well-formed name and version are sent byte for byte.
		Config: azureConfig{
			Vault: "vault1", Secret: "db-password", Version: "abc123", Token: "t",
		},
		WantPath: "/secrets/db-password/abc123",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			raw, err := json.Marshal(test.Config)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			if _, err := resolveAzure(context.Background(), string(raw)); err != nil {
				t.Fatalf("resolveAzure() error = %v", err)
			}
			path, query := got.seen()
			if diff := cmp.Diff(test.WantPath, path, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("wire path mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff("api-version="+azureAPIVersion, query); diff != "" {
				t.Errorf("wire query mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
