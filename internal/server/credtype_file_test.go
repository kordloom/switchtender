package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/run"
)

// kubeTypeBody is a custom type in AWX's kubeconfig shape: one multiline secret field written to a
// file whose path reaches the run as KUBECONFIG.
const kubeTypeBody = `{"name":"Kubeconfig","fields":[{"name":"kubeconfig","secret":true,` +
	`"multiline":true}],"file":{"template":"{{ kubeconfig }}"},` +
	`"env":{"KUBECONFIG":"{{ tower.filename }}"}}`

// kubeSecret is the token in the kubeconfig a test stores. No response may carry it.
const kubeSecret = "kube-api-token-77aa"

// TestCredentialTypeFileInjectorsThroughTheAPI proves a type with file injectors is accepted, stored,
// and returned with its file templates, and that a malformed one is refused at creation.
func TestCredentialTypeFileInjectorsThroughTheAPI(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Body     string
		WantCode int
	}{{ // Test 0: A single file referenced through tower.filename is created.
		Body: kubeTypeBody, WantCode: http.StatusCreated,
	}, { // Test 1: A file no injector hands over is refused.
		Body: `{"name":"Lost","fields":[{"name":"a"}],"file":{"template":"{{a}}"},` +
			`"env":{"A":"{{a}}"}}`,
		WantCode: http.StatusBadRequest,
	}, { // Test 2: Mixing template with template.<name> is refused.
		Body: `{"name":"Mixed","fields":[{"name":"a"}],"file":{"template":"{{a}}",` +
			`"template.b":"{{a}}"},"env":{"A":"{{tower.filename}}","B":"{{tower.filename.b}}"}}`,
		WantCode: http.StatusBadRequest,
	}, { // Test 3: Named files referenced by name are created.
		Body: `{"name":"Certs","fields":[{"name":"cert"},{"name":"key","secret":true}],` +
			`"file":{"template.cert":"{{cert}}","template.key":"{{key}}"},` +
			`"env":{"CERT":"{{tower.filename.cert}}","KEY":"{{awx.filename.key}}"}}`,
		WantCode: http.StatusCreated,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			h, _ := typedServer(t)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/credential-types",
				strings.NewReader(test.Body)))
			if rec.Code != test.WantCode {
				t.Fatalf("create = %d, want %d: %s", rec.Code, test.WantCode, rec.Body.String())
			}
			if rec.Code != http.StatusCreated {
				return
			}
			var created credential.CredentialType
			if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/credential-types/"+created.ID, nil))
			var got credential.CredentialType
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			var sent credential.CredentialType
			if err := json.Unmarshal([]byte(test.Body), &sent); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(sent.FileInjectors, got.FileInjectors); diff != "" {
				t.Errorf("file injectors did not round trip (-sent +got):\n%s", diff)
			}
		})
	}
}

// fieldsServer stands up a server whose credential and type stores the test can read directly.
func fieldsServer(t *testing.T) (http.Handler, credential.Store, *credential.Sealer, string) {
	t.Helper()
	sealer := credential.NewSealer("pass", "salt")
	types := credential.NewMemTypeStore()
	creds := credential.NewMemStore()
	var typ credential.CredentialType
	if err := json.Unmarshal([]byte(kubeTypeBody), &typ); err != nil {
		t.Fatal(err)
	}
	typ.ID = "ctype_kube"
	if err := types.Save(t.Context(), &typ); err != nil {
		t.Fatal(err)
	}
	// An imported typed credential arrives as a shell with no values.
	if err := creds.Save(t.Context(), &credential.Credential{
		ID: "cred_shell", Name: "prod-kube", TypeID: "ctype_kube",
	}); err != nil {
		t.Fatal(err)
	}
	if err := creds.Save(t.Context(), &credential.Credential{
		ID: "cred_token", Name: "tok", Kind: credential.KindToken,
	}); err != nil {
		t.Fatal(err)
	}
	h := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithCredentials(creds, sealer), WithCredentialTypes(types)).Handler()
	return h, creds, sealer, typ.ID
}

// TestTypedCredentialUpdateSetsFields proves a typed credential's values can be set in place, which
// is what an imported shell needs, without the response, the list, or the record carrying them,
// and that the single-secret shape is refused for it rather than reinterpreting its sealed object.
func TestTypedCredentialUpdateSetsFields(t *testing.T) {
	t.Parallel()
	kubeconfig := "apiVersion: v1\\nusers:\\n- user:\\n    token: " + kubeSecret + "\\n"
	tests := []struct {
		ID       string
		Body     string
		WantCode int
		WantOpen string
	}{{ // Test 0: Fields set the shell's values, sealed as one object.
		ID: "cred_shell", Body: `{"name":"prod-kube","fields":{"kubeconfig":"` + kubeconfig + `"}}`,
		WantCode: http.StatusOK,
		WantOpen: `{"kubeconfig":"apiVersion: v1\nusers:\n- user:\n    token: ` + kubeSecret + `\n"}`,
	}, { // Test 1: A field the type does not declare is refused.
		ID: "cred_shell", Body: `{"name":"prod-kube","fields":{"surprise":"x"}}`,
		WantCode: http.StatusBadRequest,
	}, { // Test 2: A single secret on a typed credential is refused.
		ID: "cred_shell", Body: `{"name":"prod-kube","secret":"raw"}`,
		WantCode: http.StatusBadRequest,
	}, { // Test 3: Fields on a built-in kind are refused.
		ID: "cred_token", Body: `{"name":"tok","fields":{"a":"b"}}`,
		WantCode: http.StatusBadRequest,
	}, { // Test 4: A rename alone keeps the type and needs no values.
		ID: "cred_shell", Body: `{"name":"renamed"}`, WantCode: http.StatusOK,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			h, creds, sealer, typeID := fieldsServer(t)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/credentials/"+test.ID,
				strings.NewReader(test.Body)))
			if rec.Code != test.WantCode {
				t.Fatalf("update = %d, want %d: %s", rec.Code, test.WantCode, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), kubeSecret) {
				t.Errorf("the update response echoed the value: %s", rec.Body.String())
			}
			stored, err := creds.Get(t.Context(), test.ID)
			if err != nil {
				t.Fatal(err)
			}
			if test.ID == "cred_shell" && stored.TypeID != typeID {
				t.Errorf("the update dropped the credential's type: %q", stored.TypeID)
			}
			if test.WantOpen != "" {
				plain, err := sealer.Open(stored.Secret)
				if err != nil {
					t.Fatalf("Open() error = %v", err)
				}
				if diff := cmp.Diff(test.WantOpen, plain); diff != "" {
					t.Errorf("sealed values mismatch (-want +got):\n%s", diff)
				}
			}
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/credentials", nil))
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), test.ID) {
				t.Fatalf("GET /v1/credentials = %d without %s, so the leak check would prove nothing: %s",
					rec.Code, test.ID, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), kubeSecret) || strings.Contains(rec.Body.String(), "apiVersion") {
				t.Errorf("GET /v1/credentials returned the credential's values: %s", rec.Body.String())
			}
		})
	}
}
