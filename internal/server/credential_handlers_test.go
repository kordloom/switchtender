package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/run"
)

// TestCreateCredentialPassphrase verifies a passphrase is sealed into an ssh_key's structured secret,
// is rejected on any other kind or an external source, and is never echoed in the response.
func TestCreateCredentialPassphrase(t *testing.T) {
	t.Parallel()
	sealer := credential.NewSealer("pass", "salt")
	tests := []struct {
		Name     string
		Body     string
		WantCode int
	}{{ // Test 0: A passphrase on a local ssh_key is accepted and sealed with the key.
		Name:     "ssh key with passphrase",
		Body:     `{"name":"k","kind":"ssh_key","secret":"KEYBODY","passphrase":"unlock"}`,
		WantCode: http.StatusCreated,
	}, { // Test 1: A passphrase on a non-ssh_key kind is rejected.
		Name:     "passphrase on env",
		Body:     `{"name":"k","kind":"env","secret":"A=b","passphrase":"unlock"}`,
		WantCode: http.StatusBadRequest,
	}, { // Test 2: A passphrase on an external-source ssh_key is rejected.
		Name:     "passphrase on command source",
		Body:     `{"name":"k","kind":"ssh_key","source":"command","secret":"cat key","passphrase":"unlock"}`,
		WantCode: http.StatusBadRequest,
	}}
	for i, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", i, test.Name), func(t *testing.T) {
			t.Parallel()
			store := credential.NewMemStore()
			handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
				WithCredentials(store, sealer)).Handler()
			req := httptest.NewRequest(http.MethodPost, "/v1/credentials", strings.NewReader(test.Body))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.WantCode {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, test.WantCode, rec.Body.String())
			}
			if rec.Code != http.StatusCreated {
				return
			}
			list, err := store.List(context.Background())
			if err != nil || len(list) != 1 {
				t.Fatalf("List() = %v, %v, want one stored credential", list, err)
			}
			plain, err := sealer.Open(list[0].Secret)
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			m := credential.ParseSSHKey(plain)
			if m.PrivateKey != "KEYBODY" || m.Passphrase != "unlock" {
				t.Errorf("stored secret = {%q, %q}, want {KEYBODY, unlock}", m.PrivateKey, m.Passphrase)
			}
			if strings.Contains(rec.Body.String(), "KEYBODY") || strings.Contains(rec.Body.String(), "unlock") {
				t.Error("create response leaked the secret or passphrase")
			}
		})
	}
}

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
