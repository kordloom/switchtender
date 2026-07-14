package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/dcadolph/railwarden/internal/credential"
	"github.com/dcadolph/railwarden/internal/run"
)

// TestListCredentialsNeedsSecret verifies the list flags a credential shell that has no sealed
// secret, as imports create, and does not flag one whose secret is set, without leaking either
// secret.
func TestListCredentialsNeedsSecret(t *testing.T) {
	t.Parallel()
	store := credential.NewMemStore()
	ctx := context.Background()
	if err := store.Save(ctx, &credential.Credential{ID: "cred_shell", Name: "imported", Kind: credential.KindEnv}); err != nil {
		t.Fatalf("save shell: %v", err)
	}
	if err := store.Save(ctx, &credential.Credential{ID: "cred_full", Name: "ready", Kind: credential.KindEnv, Secret: "sealed"}); err != nil {
		t.Fatalf("save full: %v", err)
	}

	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithCredentials(store, credential.NewSealer("pass", "salt"))).Handler()
	req := httptest.NewRequest(http.MethodGet, "/v1/credentials", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Credentials []struct {
			ID          string `json:"id"`
			Secret      string `json:"secret"`
			NeedsSecret bool   `json:"needs_secret"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[string]bool{}
	for _, c := range resp.Credentials {
		got[c.ID] = c.NeedsSecret
		if c.Secret != "" {
			t.Errorf("credential %q leaked a secret in the list response", c.ID)
		}
	}
	if !got["cred_shell"] {
		t.Error("imported shell should report needs_secret=true")
	}
	if got["cred_full"] {
		t.Error("credential with a secret should report needs_secret=false")
	}
}
