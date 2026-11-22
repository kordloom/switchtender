package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/user"
)

// TestARegistryLoginMovesHostOnlyWithItsSecret proves a caller below admin cannot point a stored
// registry login at a new host without sending the login in the same request. The login goes only
// to the host it is bound to, so moving the host alone carried it to a server the caller chose on
// the next pull. Sending the secret, or being an admin, still moves it, and Docker Hub's names are
// one host at write as they are at use.
func TestARegistryLoginMovesHostOnlyWithItsSecret(t *testing.T) {
	t.Parallel()
	operator := Actor{UserID: "user_op", Name: "op", Role: user.RoleOperator}
	admin := Actor{UserID: "user_admin", Name: "admin", Role: user.RoleAdmin}
	tests := []struct {
		// StoredHost is the host the stored login is bound to.
		StoredHost string
		// Actor is the caller.
		Actor Actor
		// Body is the update request JSON.
		Body string
		// WantCode is the response status.
		WantCode int
		// WantHost is the host stored after the request.
		WantHost string
	}{{ // Test 0: An operator moving the host without the secret is refused.
		StoredHost: "ghcr.io", Actor: operator,
		Body:     `{"name":"ghcr login","settings":{"registry_host":"evil.example"}}`,
		WantCode: http.StatusBadRequest, WantHost: "ghcr.io",
	}, { // Test 1: The same move with the login in the request is accepted.
		StoredHost: "ghcr.io", Actor: operator,
		Body: `{"name":"ghcr login","secret":"username=bot\npassword=new",` +
			`"settings":{"registry_host":"evil.example"}}`,
		WantCode: http.StatusOK, WantHost: "evil.example",
	}, { // Test 2: An admin moves the host without the secret.
		StoredHost: "ghcr.io", Actor: admin,
		Body:     `{"name":"ghcr login","settings":{"registry_host":"registry.example.com"}}`,
		WantCode: http.StatusOK, WantHost: "registry.example.com",
	}, { // Test 3: docker.io to index.docker.io is the same host, as a pull judges it.
		StoredHost: "docker.io", Actor: operator,
		Body:     `{"name":"ghcr login","settings":{"registry_host":"index.docker.io"}}`,
		WantCode: http.StatusOK, WantHost: "index.docker.io",
	}, { // Test 4: A case change is the same host.
		StoredHost: "ghcr.io", Actor: operator,
		Body:     `{"name":"ghcr login","settings":{"registry_host":"GHCR.io"}}`,
		WantCode: http.StatusOK, WantHost: "GHCR.io",
	}, { // Test 5: A rename that keeps the stored settings is accepted.
		StoredHost: "ghcr.io", Actor: operator,
		Body:     `{"name":"renamed"}`,
		WantCode: http.StatusOK, WantHost: "ghcr.io",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			sealer := sharedSealer("pass", "salt")
			sealed, err := sealer.Seal("username=bot\npassword=pw")
			if err != nil {
				t.Fatalf("Seal() error = %v", err)
			}
			store := credential.NewMemStore()
			if err := store.Save(ctx, &credential.Credential{ID: "cred_reg", Name: "ghcr login",
				Kind: credential.KindRegistry, Secret: sealed, CreatedAt: time.Now(),
				Settings: map[string]string{credential.SettingRegistryHost: test.StoredHost}}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			handler := updateCredentialHandler(store, nil, sealer, nil, zap.NewNop())
			req := httptest.NewRequest(http.MethodPut, "/v1/credentials/cred_reg",
				strings.NewReader(test.Body))
			req.SetPathValue("id", "cred_reg")
			req = req.WithContext(context.WithValue(req.Context(), actorKey{}, test.Actor))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if diff := cmp.Diff(test.WantCode, rec.Code); diff != "" {
				t.Fatalf("status mismatch (-want +got):\n%s\nbody: %s", diff, rec.Body.String())
			}
			got, err := store.Get(ctx, "cred_reg")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if diff := cmp.Diff(test.WantHost, got.RegistryHost()); diff != "" {
				t.Errorf("stored host mismatch (-want +got):\n%s", diff)
			}
			if test.WantCode != http.StatusOK && !strings.Contains(rec.Body.String(), "login in the same") {
				t.Errorf("body = %s, want it to say the login must come with the host", rec.Body.String())
			}
		})
	}
}
