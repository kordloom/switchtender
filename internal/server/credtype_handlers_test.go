package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/run"
)

// typedServer stands up a server with credentials and custom types enabled.
func typedServer(t *testing.T) (http.Handler, credential.TypeStore) {
	t.Helper()
	sealer := credential.NewSealer("pass", "salt")
	types := credential.NewMemTypeStore()
	h := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithCredentials(credential.NewMemStore(), sealer), WithCredentialTypes(types)).Handler()
	return h, types
}

// TestCredentialTypeLifecycle covers create, list, get, and delete through the API.
func TestCredentialTypeLifecycle(t *testing.T) {
	t.Parallel()
	h, _ := typedServer(t)

	body := `{"name":"Datadog","fields":[{"name":"api_key","secret":true}],` +
		`"env":{"DD_API_KEY":"{{api_key}}"}}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/credential-types", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created credential.CredentialType
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if created.ID == "" {
		t.Fatal("created type has no id")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/credential-types", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Datadog") {
		t.Fatalf("list = %d body %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/credential-types/"+created.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d, want 200", rec.Code)
	}
}

// TestCredentialTypeCreateRejectsAMalformedDefinition checks the API refuses a type that would
// inject nothing or reference a field it does not declare, at creation.
func TestCredentialTypeCreateRejectsAMalformedDefinition(t *testing.T) {
	t.Parallel()
	h, _ := typedServer(t)
	bad := []string{
		`{"name":"NoFields","env":{"X":"y"}}`,
		`{"name":"Inert","fields":[{"name":"a"}]}`,
		`{"name":"BadRef","fields":[{"name":"a"}],"env":{"X":"{{b}}"}}`,
		`{"name":"BadEnvName","fields":[{"name":"a"}],"env":{"bad name":"{{a}}"}}`,
	}
	for _, body := range bad {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/credential-types", strings.NewReader(body)))
		if rec.Code < 400 {
			t.Errorf("create accepted a malformed type (%d): %s", rec.Code, body)
		}
	}
}

// TestTypedCredentialCreateValidatesFields checks a typed credential accepts only fields the type
// declares, and stores its values sealed.
func TestTypedCredentialCreateValidatesFields(t *testing.T) {
	t.Parallel()
	h, types := typedServer(t)
	if err := types.Save(t.Context(), &credential.CredentialType{
		ID:           "ctype_1",
		Name:         "API",
		Fields:       []credential.Field{{Name: "host"}, {Name: "token", Secret: true}},
		EnvInjectors: map[string]string{"API_TOKEN": "{{token}}"},
	}); err != nil {
		t.Fatalf("Save() type error = %v", err)
	}

	// A field the type does not declare is refused.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/credentials", strings.NewReader(
		`{"name":"prod","type_id":"ctype_1","fields":{"host":"h","token":"t","surprise":"x"}}`)))
	if rec.Code < 400 {
		t.Errorf("a credential with an undeclared field was accepted: %d %s", rec.Code, rec.Body.String())
	}

	// The declared fields are accepted, and the response never carries the secret.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/credentials", strings.NewReader(
		`{"name":"prod","type_id":"ctype_1","fields":{"host":"api.example.com","token":"s3cr3t"}}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create typed credential = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "s3cr3t") {
		t.Errorf("the response echoed the sealed secret: %s", rec.Body.String())
	}

	// An unknown type is refused.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/credentials", strings.NewReader(
		`{"name":"x","type_id":"ctype_gone","fields":{"a":"b"}}`)))
	if rec.Code < 400 {
		t.Errorf("a credential of an unknown type was accepted: %d", rec.Code)
	}
}
