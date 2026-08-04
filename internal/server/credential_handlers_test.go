package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestCreateCredentialVaultID(t *testing.T) {
	t.Parallel()
	sealer := credential.NewSealer("pass", "salt")
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Body is the request JSON.
		Body string
		// WantCode is the expected status.
		WantCode int
		// WantVaultID is the stored label on a created credential.
		WantVaultID string
	}{{ // Test 0: A labeled vault password stores its label.
		Name:     "labeled vault",
		Body:     `{"name":"v","kind":"vault_password","secret":"pw","vault_id":"prod"}`,
		WantCode: http.StatusCreated, WantVaultID: "prod",
	}, { // Test 1: An unlabeled vault password is the classic case.
		Name:     "unlabeled vault",
		Body:     `{"name":"v","kind":"vault_password","secret":"pw"}`,
		WantCode: http.StatusCreated, WantVaultID: "",
	}, { // Test 2: A label on any other kind is refused, since --vault-id means nothing there.
		Name:     "label on env",
		Body:     `{"name":"v","kind":"env","secret":"A=b","vault_id":"prod"}`,
		WantCode: http.StatusBadRequest,
	}, { // Test 3: A label with a separator or spaces is refused before it reaches an argument.
		Name:     "hostile label",
		Body:     `{"name":"v","kind":"vault_password","secret":"pw","vault_id":"a@b c"}`,
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
				t.Fatalf("List() = %v, %v, want one credential", list, err)
			}
			if list[0].VaultID != test.WantVaultID {
				t.Errorf("stored vault_id = %q, want %q", list[0].VaultID, test.WantVaultID)
			}
		})
	}
}

// sealedVaultCred stores a vault_password credential with a label, returning the store.
func sealedVaultCred(t *testing.T, sealer *credential.Sealer, label string) credential.Store {
	t.Helper()
	store := credential.NewMemStore()
	sealed, err := sealer.Seal("the-password")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	if err := store.Save(context.Background(), &credential.Credential{
		ID: "cred_1", Name: "prod-vault", Kind: credential.KindVaultPassword,
		VaultID: label, Secret: sealed, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	return store
}

func TestUpdateCredentialVaultID(t *testing.T) {
	t.Parallel()
	sealer := credential.NewSealer("pass", "salt")

	tests := []struct {
		// Name says what the case proves.
		Name string
		// Body is the PUT payload.
		Body string
		// WantCode is the expected status.
		WantCode int
		// WantVaultID is the stored label afterward, checked on a 200.
		WantVaultID string
		// WantKind is the stored kind afterward, checked on a 200.
		WantKind credential.Kind
	}{{ // Test 0: A rename that omits vault_id keeps the stored label; wiping it would break the
		// multi-vault run the label drives. This is the reported regression.
		Name: "rename keeps the label", Body: `{"name":"renamed"}`,
		WantCode: http.StatusOK, WantVaultID: "prod", WantKind: credential.KindVaultPassword,
	}, { // Test 1: An explicit empty vault_id clears the label.
		Name: "explicit empty clears", Body: `{"name":"prod-vault","vault_id":""}`,
		WantCode: http.StatusOK, WantVaultID: "", WantKind: credential.KindVaultPassword,
	}, { // Test 2: A new label relabels.
		Name: "relabel", Body: `{"name":"prod-vault","vault_id":"staging"}`,
		WantCode: http.StatusOK, WantVaultID: "staging", WantKind: credential.KindVaultPassword,
	}, { // Test 3: Changing kind to a non-vault kind, with a secret, drops the stale label rather
		// than storing a label on a non-vault credential.
		Name: "kind change drops label", Body: `{"name":"prod-vault","kind":"env","secret":"A=b"}`,
		WantCode: http.StatusOK, WantVaultID: "", WantKind: credential.KindEnv,
	}, { // Test 4: Claiming kind=vault_password with a label but no secret must not persist the
		// label while the stored kind stays put; create rejects exactly this state.
		Name: "label on kind change without secret", Body: `{"name":"x","kind":"vault_password","vault_id":"prod"}`,
		WantCode: http.StatusBadRequest,
	}, { // Test 5: A hostile label is refused before it can reach a --vault-id argument.
		Name: "hostile label", Body: `{"name":"prod-vault","vault_id":"a@b c"}`,
		WantCode: http.StatusBadRequest,
	}}
	for i, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", i, test.Name), func(t *testing.T) {
			t.Parallel()
			// The stored credential starts as an ssh_key for test 4, so a kind claim of
			// vault_password without a secret leaves it non-vault; every other case starts vault.
			var store credential.Store
			if test.Name == "label on kind change without secret" {
				store = credential.NewMemStore()
				sealed, _ := sealer.Seal("KEYBODY")
				_ = store.Save(context.Background(), &credential.Credential{
					ID: "cred_1", Name: "k", Kind: credential.KindSSHKey, Secret: sealed,
					CreatedAt: time.Now(),
				})
			} else {
				store = sealedVaultCred(t, sealer, "prod")
			}
			handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
				WithCredentials(store, sealer)).Handler()
			req := httptest.NewRequest(http.MethodPut, "/v1/credentials/cred_1", strings.NewReader(test.Body))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.WantCode {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, test.WantCode, rec.Body.String())
			}
			if rec.Code != http.StatusOK {
				return
			}
			got, err := store.Get(context.Background(), "cred_1")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.VaultID != test.WantVaultID {
				t.Errorf("stored vault_id = %q, want %q", got.VaultID, test.WantVaultID)
			}
			if got.Kind != test.WantKind {
				t.Errorf("stored kind = %q, want %q", got.Kind, test.WantKind)
			}
		})
	}
}
