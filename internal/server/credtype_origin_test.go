package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// lostFileBody is a type writing one file whose path no injector hands over, claiming to be an AWX
// import. The claim is the server's to make, not the request's.
const lostFileBody = `{"name":"Legacy","fields":[{"name":"host"},` +
	`{"name":"bundle","secret":true}],"file":{"template":"{{ bundle }}"},` +
	`"env":{"BUNDLE_HOST":"{{ host }}"},"origin":"awx"}`

// TestCredentialTypeOriginIsTheServers proves a request cannot claim the allowance an AWX import
// has: a type created through the API is refused for a file nothing references, with a message
// naming the file and the reference that fixes it, whatever origin the body asserts, and a type
// that is valid is stored with no origin.
func TestCredentialTypeOriginIsTheServers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Body        string
		WantCode    int
		WantPhrases []string
	}{{ // Test 0: A file nothing references is refused, naming the file and the fix.
		Body: lostFileBody, WantCode: http.StatusBadRequest,
		WantPhrases: []string{
			"file template is written but no env or extra-var injector references its path",
			"Hand the path over with {{ tower.filename }}",
			`for example \"env\": {\"CREDENTIAL_FILE\": \"{{ tower.filename }}\"}`,
		},
	}, { // Test 1: A valid type claiming an AWX origin is stored without one.
		Body: strings.Replace(lostFileBody, `"env":{"BUNDLE_HOST":"{{ host }}"}`,
			`"env":{"BUNDLE_HOST":"{{ host }}","BUNDLE_FILE":"{{ tower.filename }}"}`, 1),
		WantCode: http.StatusCreated,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			h, types := typedServer(t)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/credential-types",
				strings.NewReader(test.Body)))
			if rec.Code != test.WantCode {
				t.Fatalf("create = %d, want %d: %s", rec.Code, test.WantCode, rec.Body.String())
			}
			for _, phrase := range test.WantPhrases {
				if !strings.Contains(rec.Body.String(), phrase) {
					t.Errorf("the refusal does not say %q: %s", phrase, rec.Body.String())
				}
			}
			stored, err := types.List(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, typ := range stored {
				if typ.Origin != "" {
					t.Errorf("a type created through the API was stored with origin %q", typ.Origin)
				}
			}
			if test.WantCode == http.StatusCreated && len(stored) != 1 {
				t.Errorf("stored %d types, want the one created", len(stored))
			}
		})
	}
}

// TestImportedTypeEditKeepsWhatItArrivedWith proves an edit of an imported type keeps the file it
// arrived with that nothing references, its origin, and its creation time, while it cannot add a
// file of the same kind. The same edits of a type created here are refused.
func TestImportedTypeEditKeepsWhatItArrivedWith(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	imported := &credential.CredentialType{
		ID: "ctype_legacy", Name: "Legacy", Origin: credential.OriginAWX, CreatedAt: created,
		Fields:        []credential.Field{{Name: "host"}, {Name: "bundle", Secret: true}},
		FileInjectors: map[string]string{"template.ca": "{{ bundle }}", "template.key": "{{ bundle }}"},
		EnvInjectors:  map[string]string{"BUNDLE_CA": "{{ tower.filename.ca }}"},
	}
	edit := func(name, env string) string {
		return `{"name":"` + name + `","fields":[{"name":"host"},{"name":"bundle","secret":true}],` +
			`"file":{"template.ca":"{{ bundle }}","template.key":"{{ bundle }}"},"env":` + env +
			`,"origin":""}`
	}
	tests := []struct {
		Origin     string
		Body       string
		WantCode   int
		WantPhrase string
	}{{ // Test 0: A rename keeps the key nothing references, and the body's empty origin is ignored.
		Origin: credential.OriginAWX, Body: edit("Legacy v2", `{"BUNDLE_CA":"{{ tower.filename.ca }}"}`),
		WantCode: http.StatusOK,
	}, { // Test 1: Dropping the reference to ca adds a file nothing references, which is refused.
		Origin: credential.OriginAWX, Body: edit("Legacy v2", `{"HOST":"{{ host }}"}`),
		WantCode: http.StatusBadRequest, WantPhrase: "file ca is written",
	}, { // Test 2: The same rename of a type created here is refused for the key.
		Origin: "", Body: edit("Legacy v2", `{"BUNDLE_CA":"{{ tower.filename.ca }}"}`),
		WantCode: http.StatusBadRequest, WantPhrase: "file key is written",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			h, types := typedServer(t)
			seed := *imported
			seed.Origin = test.Origin
			if err := types.Save(t.Context(), &seed); err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/credential-types/ctype_legacy",
				strings.NewReader(test.Body)))
			if rec.Code != test.WantCode {
				t.Fatalf("update = %d, want %d: %s", rec.Code, test.WantCode, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), test.WantPhrase) {
				t.Errorf("the response does not say %q: %s", test.WantPhrase, rec.Body.String())
			}
			got, err := types.Get(t.Context(), "ctype_legacy")
			if err != nil {
				t.Fatal(err)
			}
			if got.Origin != test.Origin || !got.CreatedAt.Equal(created) {
				t.Errorf("after the edit origin = %q, created = %v, want %q and %v", got.Origin,
					got.CreatedAt, test.Origin, created)
			}
			wantName := "Legacy"
			if test.WantCode == http.StatusOK {
				wantName = "Legacy v2"
			}
			if got.Name != wantName {
				t.Errorf("name = %q, want %q", got.Name, wantName)
			}
		})
	}
}

// TestTypedCredentialSwitchesToABuiltinKind proves the one-step switch the import report gives a
// credential of a type shaped like a kubeconfig: naming the kind and carrying its secret moves the
// credential off its type in one request, sealed as that kind, and every half-measure is refused
// with the reason rather than reinterpreting the sealed field object.
func TestTypedCredentialSwitchesToABuiltinKind(t *testing.T) {
	t.Parallel()
	kubeconfig := "apiVersion: v1\\nkind: Config\\nusers:\\n- user:\\n    token: " + kubeSecret + "\\n"
	tests := []struct {
		Body       string
		WantCode   int
		WantPhrase string
		WantKind   credential.Kind
		WantOpen   string
	}{{ // Test 0: Kind and secret together switch the credential to the built-in kind.
		Body:     `{"name":"prod-kube","kind":"kubeconfig","secret":"` + kubeconfig + `"}`,
		WantCode: http.StatusOK, WantKind: credential.KindKubeconfig,
		WantOpen: "apiVersion: v1\nkind: Config\nusers:\n- user:\n    token: " + kubeSecret + "\n",
	}, { // Test 1: A kind without its secret is refused, since the field values do not fit it.
		Body:     `{"name":"prod-kube","kind":"kubeconfig"}`,
		WantCode: http.StatusBadRequest,
		WantPhrase: "moving a credential off its custom type to kubeconfig needs the kubeconfig " +
			"secret in the same request",
	}, { // Test 2: Fields beside a kind are refused as ambiguous.
		Body: `{"name":"prod-kube","kind":"kubeconfig","secret":"` + kubeconfig + `",` +
			`"fields":{"kubeconfig":"x"}}`,
		WantCode: http.StatusBadRequest, WantPhrase: "not both",
	}, { // Test 3: A federated kind holds no secret, so a stored credential does not become one.
		Body:     `{"name":"prod-kube","kind":"oidc_token","secret":"x"}`,
		WantCode: http.StatusConflict, WantPhrase: "create a new credential for oidc_token",
	}, { // Test 4: A kind that does not exist is refused.
		Body:     `{"name":"prod-kube","kind":"teleport","secret":"x"}`,
		WantCode: http.StatusBadRequest, WantPhrase: "kind must be one of",
	}, { // Test 5: A secret without a kind still keeps to the type, and is refused.
		Body:     `{"name":"prod-kube","secret":"raw"}`,
		WantCode: http.StatusBadRequest, WantPhrase: "send kind with that kind's secret",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			h, creds, sealer, typeID := fieldsServer(t)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/credentials/cred_shell",
				strings.NewReader(test.Body)))
			if rec.Code != test.WantCode {
				t.Fatalf("update = %d, want %d: %s", rec.Code, test.WantCode, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), test.WantPhrase) {
				t.Errorf("the response does not say %q: %s", test.WantPhrase, rec.Body.String())
			}
			stored, err := creds.Get(t.Context(), "cred_shell")
			if err != nil {
				t.Fatal(err)
			}
			if test.WantKind == "" {
				if stored.TypeID != typeID || stored.Kind != "" || stored.Secret != "" {
					t.Errorf("a refused switch changed the credential: type %q, kind %q",
						stored.TypeID, stored.Kind)
				}
				return
			}
			if stored.TypeID != "" || stored.Kind != test.WantKind {
				t.Errorf("after the switch type = %q, kind = %q, want no type and %q",
					stored.TypeID, stored.Kind, test.WantKind)
			}
			plain, err := sealer.Open(stored.Secret)
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			if diff := cmp.Diff(test.WantOpen, plain); diff != "" {
				t.Errorf("sealed secret mismatch (-want +got):\n%s", diff)
			}
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/credentials", nil))
			if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), kubeSecret) ||
				!strings.Contains(rec.Body.String(), `"kind":"kubeconfig"`) {
				t.Errorf("GET /v1/credentials = %d: %s, want the switched kind and no document",
					rec.Code, rec.Body.String())
			}
		})
	}
}

// TestImportApplyLogsUnreferencedFiles proves the import-time log: applying an export whose custom
// type writes a file nothing references logs a warning naming the type and the file, with neither a
// template nor a value in it, and the response still reports the type as created.
func TestImportApplyLogsUnreferencedFiles(t *testing.T) {
	t.Parallel()
	export := `{"credential_types": [{"name": "Legacy Bundle", "kind": "cloud",
		"inputs": {"fields": [{"id": "host"}, {"id": "bundle", "secret": true}]},
		"injectors": {"file": {"template": "{{ bundle }}"}, "env": {"BUNDLE_HOST": "{{ host }}"}}}]}`
	core, logs := observer.New(zapcore.DebugLevel)
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.New(core),
		WithProjects(project.NewMemStore()),
		WithInventories(inventory.NewMemStore()),
		WithCredentials(credential.NewMemStore(), nil),
		WithCredentialTypes(credential.NewMemTypeStore()),
		WithTemplates(template.NewMemStore()),
		WithSchedules(schedule.NewMemStore()),
	).Handler()
	for _, target := range []string{"/v1/import/awx", "/v1/import/awx?apply=true"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, target, strings.NewReader(export)))
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s = %d: %s", target, rec.Code, rec.Body.String())
		}
		var resp struct {
			CredentialTypes []string `json:"credential_types"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff([]string{"Legacy Bundle"}, resp.CredentialTypes); diff != "" {
			t.Errorf("POST %s credential types (-want +got):\n%s", target, diff)
		}
	}
	var warned []observer.LoggedEntry
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "no injector references") {
			warned = append(warned, e)
		}
	}
	// A preview imports nothing, so only the apply logs.
	if len(warned) != 1 || warned[0].Level != zapcore.WarnLevel {
		t.Fatalf("logged %d warnings about the file, want one at warn level, from the apply",
			len(warned))
	}
	fields := warned[0].ContextMap()
	if fields["credential_type"] != "Legacy Bundle" || fields["file"] != credential.SingleFile ||
		fields["credential_type_id"] == "" {
		t.Errorf("log fields = %v, want the type, its id, and the file", fields)
	}
	line, _ := json.Marshal(fields)
	if strings.Contains(string(line), "{{") {
		t.Errorf("the log carries a template: %s", line)
	}
}
