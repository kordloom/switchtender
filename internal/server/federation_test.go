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

	"github.com/go-jose/go-jose/v4"
	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/federation"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// fedTestSealer seals the federation keys and credentials the server tests store.
var fedTestSealer = credential.NewSealer("server-federation-pass", "server-federation-salt")

// federatedServer returns a handler with the token gate enforcing and federation on, the issuer
// behind it, and the plain text of an admin token.
func federatedServer(t *testing.T) (http.Handler, *federation.Issuer, string) {
	t.Helper()
	issuer, err := federation.NewIssuer("https://st.example.com", federation.NewMemKeyStore(),
		fedTestSealer)
	if err != nil {
		t.Fatalf("NewIssuer() error = %v", err)
	}
	if err := issuer.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	tokens := auth.NewMemStore()
	plain, tok, err := auth.New("admin")
	if err != nil {
		t.Fatalf("auth.New() error = %v", err)
	}
	if err := tokens.Save(context.Background(), tok); err != nil {
		t.Fatalf("tokens.Save() error = %v", err)
	}
	h := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), WithTokens(tokens),
		WithFederation(issuer)).Handler()
	return h, issuer, plain
}

// TestFederationDocumentsArePublic pins that a cloud with no account here reads the discovery
// document and the key set while the gate enforces tokens everywhere else, that the key set carries
// no private parameter, and that the key listing and rotation still need a token.
func TestFederationDocumentsArePublic(t *testing.T) {
	t.Parallel()
	h, issuer, plain := federatedServer(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/openid-configuration", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("discovery status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var disc federation.Discovery
	if err := json.Unmarshal(rec.Body.Bytes(), &disc); err != nil {
		t.Fatalf("decode discovery: %v", err)
	}
	if disc.Issuer != issuer.URL() || disc.JWKSURI != issuer.URL()+"/.well-known/jwks.json" {
		t.Errorf("discovery = %+v", disc)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "max-age") {
		t.Errorf("discovery Cache-Control = %q, want a short public max-age", got)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("jwks status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(rec.Body.Bytes(), &set); err != nil {
		t.Fatalf("decode jwks: %v", err)
	}
	if len(set.Keys) != 1 || !set.Keys[0].IsPublic() {
		t.Errorf("jwks = %d keys, want the one public signing key", len(set.Keys))
	}
	var raw struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode jwks: %v", err)
	}
	if _, leaked := raw.Keys[0]["d"]; leaked {
		t.Error("the key set publishes the private exponent")
	}

	for _, probe := range []struct {
		Method, Path string
	}{{http.MethodGet, "/v1/federation/keys"}, {http.MethodPost, "/v1/federation/keys/rotate"}} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(probe.Method, probe.Path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token = %d, want 401", probe.Method, probe.Path, rec.Code)
		}
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/federation/keys/rotate", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &set); err != nil {
		t.Fatalf("decode jwks: %v", err)
	}
	if len(set.Keys) != 2 {
		t.Errorf("jwks after a rotation = %d keys, want the new key and the one it replaced",
			len(set.Keys))
	}
}

// TestFederationRoutesWithoutAnIssuer pins that an install with federation off answers 404 with the
// flag to set, rather than an empty document a cloud would read as an issuer with no keys.
func TestFederationRoutesWithoutAnIssuer(t *testing.T) {
	t.Parallel()
	h := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop()).Handler()
	for _, path := range []string{"/.well-known/openid-configuration", "/.well-known/jwks.json",
		"/v1/federation/keys"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound ||
			!strings.Contains(rec.Body.String(), "--federation-issuer") {
			t.Errorf("GET %s = %d %q, want 404 naming the flag", path, rec.Code, rec.Body.String())
		}
	}
}

// callCredential sends body to the credential handler for method as an admin and returns the
// recorder. A PUT targets cred_1.
func callCredential(t *testing.T, store credential.Store, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	var h http.HandlerFunc
	path := "/v1/credentials"
	if method == http.MethodPut {
		h = updateCredentialHandler(store, nil, fedTestSealer, &authorizer{}, zap.NewNop())
		path += "/cred_1"
	} else {
		h = createCredentialHandler(store, nil, fedTestSealer, &authorizer{}, zap.NewNop())
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.SetPathValue("id", "cred_1")
	req = req.WithContext(context.WithValue(req.Context(), actorKey{},
		Actor{UserID: "u", Role: user.RoleAdmin}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestCreateFederatedCredential pins that a federated credential is stored with settings and no
// secret, and that a secret, a source, or settings that would not mint are refused when it is
// saved.
func TestCreateFederatedCredential(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Body     string
		WantCode int
	}{{ // Test 0: Settings and no secret are stored.
		Body: `{"name":"aws-prod","kind":"aws_oidc",` +
			`"settings":{"role_arn":"arn:aws:iam::123456789012:role/deploy","environment":"prod"}}`,
		WantCode: http.StatusCreated,
	}, { // Test 1: A secret is refused, since nothing durable is stored.
		Body: `{"name":"aws-prod","kind":"aws_oidc","secret":"AKIA...",` +
			`"settings":{"role_arn":"arn:aws:iam::123456789012:role/deploy"}}`,
		WantCode: http.StatusBadRequest,
	}, { // Test 2: A source is refused, since there is nothing to resolve.
		Body:     `{"name":"tok","kind":"oidc_token","source":"vault","settings":{"audience":"vault"}}`,
		WantCode: http.StatusBadRequest,
	}, { // Test 3: A misspelled setting is refused.
		Body: `{"name":"aws-prod","kind":"aws_oidc",` +
			`"settings":{"role":"arn:aws:iam::123456789012:role/deploy"}}`,
		WantCode: http.StatusBadRequest,
	}, { // Test 4: A required setting that is missing is refused.
		Body: `{"name":"gcp","kind":"gcp_oidc",` +
			`"settings":{"service_account":"a@b.iam.gserviceaccount.com"}}`,
		WantCode: http.StatusBadRequest,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := credential.NewMemStore()
			rec := callCredential(t, store, http.MethodPost, test.Body)
			if rec.Code != test.WantCode {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, test.WantCode, rec.Body.String())
			}
			list, err := store.List(context.Background())
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if test.WantCode != http.StatusCreated {
				if len(list) != 0 {
					t.Error("a refused credential was stored")
				}
				return
			}
			if len(list) != 1 || list[0].Secret != "" || list[0].Settings["environment"] != "prod" {
				t.Errorf("stored credential = %+v, want settings and no secret", list[0])
			}
		})
	}
}

// TestFederatedCredentialListsAsNeedingNothing pins that the list does not flag a federated
// credential as waiting for a secret it will never have.
func TestFederatedCredentialListsAsNeedingNothing(t *testing.T) {
	t.Parallel()
	store := credential.NewMemStore()
	if err := store.Save(context.Background(), &credential.Credential{
		ID: "cred_1", Name: "tok", Kind: credential.KindOIDCToken, CreatedAt: time.Now(),
		Settings: map[string]string{"audience": "vault"},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	rec := httptest.NewRecorder()
	listCredentialsHandler(store, fedTestSealer, nil, &authorizer{}, zap.NewNop()).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/v1/credentials", nil))
	var got struct {
		Credentials []struct {
			NeedsSecret bool `json:"needs_secret"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(got.Credentials) != 1 || got.Credentials[0].NeedsSecret {
		t.Errorf("list = %s, want the federated credential not waiting for a secret", rec.Body.String())
	}
}

// TestUpdateFederatedCredential pins the update rules: settings are checked against the kind, a
// secret is refused, and a stored credential is not turned into a federated one in place.
func TestUpdateFederatedCredential(t *testing.T) {
	t.Parallel()
	sealed, err := fedTestSealer.Seal("API_TOKEN=x")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	federated := &credential.Credential{ID: "cred_1", Name: "aws", Kind: credential.KindAWSOIDC,
		Settings: map[string]string{"role_arn": "arn:aws:iam::123456789012:role/deploy"}}
	stored := &credential.Credential{ID: "cred_1", Name: "env", Kind: credential.KindEnv,
		Secret: sealed}
	tests := []struct {
		Existing *credential.Credential
		Body     string
		WantCode int
	}{{ // Test 0: New settings that mint are saved.
		Existing: federated, WantCode: http.StatusOK,
		Body: `{"name":"aws","settings":` +
			`{"role_arn":"arn:aws:iam::123456789012:role/other","region":"us-east-2"}}`,
	}, { // Test 1: Settings that would not mint are refused.
		Existing: federated, WantCode: http.StatusBadRequest,
		Body: `{"name":"aws","settings":` +
			`{"role_arn":"arn:aws:iam::123456789012:role/other","unknown_key":"x"}}`,
	}, { // Test 2: A secret is refused.
		Existing: federated, WantCode: http.StatusBadRequest,
		Body: `{"name":"aws","secret":"something"}`,
	}, { // Test 3: A stored credential does not become a federated one.
		Existing: stored, WantCode: http.StatusConflict,
		Body: `{"name":"env","kind":"oidc_token","secret":"x","settings":{"audience":"vault"}}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := credential.NewMemStore()
			existing := *test.Existing
			if err := store.Save(context.Background(), &existing); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			rec := callCredential(t, store, http.MethodPut, test.Body)
			if rec.Code != test.WantCode {
				t.Errorf("status = %d, want %d (%s)", rec.Code, test.WantCode, rec.Body.String())
			}
		})
	}
}

// TestDoctorJudgesAFederatedCredentialBySettings pins that the doctor does not report a federated
// credential as missing a secret it never has, and does report one whose settings would not mint.
func TestDoctorJudgesAFederatedCredentialBySettings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Settings     map[string]string
		WantFindings []string
	}{{ // Test 0: Settings that mint leave nothing to report.
		Settings:     map[string]string{"role_arn": "arn:aws:iam::123456789012:role/deploy"},
		WantFindings: nil,
	}, { // Test 1: Settings that would not mint are broken.
		Settings:     map[string]string{"role_arn": "deploy"},
		WantFindings: []string{"broken"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := credential.NewMemStore()
			if err := store.Save(context.Background(), &credential.Credential{
				ID: "cred_1", Name: "aws", Kind: credential.KindAWSOIDC, Settings: test.Settings,
				CreatedAt: time.Now(),
			}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			rec := httptest.NewRecorder()
			doctorHandler(nil, nil, store, nil, nil, func() bool { return true }, nil, nil,
				zap.NewNop()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/doctor", nil))
			var report doctorReport
			if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
				t.Fatalf("decode report: %v", err)
			}
			var got []string
			for _, f := range report.Findings {
				if f.ObjectType == "credential" {
					got = append(got, f.Severity)
				}
			}
			if diff := cmp.Diff(test.WantFindings, got); diff != "" {
				t.Errorf("credential findings mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
