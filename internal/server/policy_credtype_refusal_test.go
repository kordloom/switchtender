package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/policy"
)

// stubPolicies is a policy.Store that answers Get and List from fixed values and fails whichever
// calls a test asks it to, so the handlers' store-error and read-only paths are reachable.
type stubPolicies struct {
	// present is what Get answers with, nil to answer policy.ErrNotFound.
	present *policy.Policy
	// list is what List answers with when listErr is unset.
	list []*policy.Policy
	// getErr, listErr, saveErr, and deleteErr replace the ordinary answer of the method they name.
	getErr    error
	listErr   error
	saveErr   error
	deleteErr error
}

// Save reports the configured save failure.
func (s *stubPolicies) Save(context.Context, *policy.Policy) error { return s.saveErr }

// Get answers with the fixed policy, its configured error, or policy.ErrNotFound.
func (s *stubPolicies) Get(context.Context, string) (*policy.Policy, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.present == nil {
		return nil, policy.ErrNotFound
	}
	return s.present, nil
}

// List answers with the fixed list or the configured list failure.
func (s *stubPolicies) List(context.Context) ([]*policy.Policy, error) {
	return s.list, s.listErr
}

// Delete reports the configured delete failure.
func (s *stubPolicies) Delete(context.Context, string) error { return s.deleteErr }

// stubCredTypes is a credential.TypeStore that answers Get from a fixed value and fails whichever
// calls a test asks it to.
type stubCredTypes struct {
	// present is what Get answers with, nil to answer credential.ErrNotFound.
	present *credential.CredentialType
	// getErr, listErr, saveErr, and deleteErr replace the ordinary answer of the method they name.
	getErr    error
	listErr   error
	saveErr   error
	deleteErr error
}

// Save reports the configured save failure.
func (s *stubCredTypes) Save(context.Context, *credential.CredentialType) error {
	return s.saveErr
}

// Get answers with the fixed type, its configured error, or credential.ErrNotFound.
func (s *stubCredTypes) Get(context.Context, string) (*credential.CredentialType, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.present == nil {
		return nil, credential.ErrNotFound
	}
	return s.present, nil
}

// List reports the configured list failure.
func (s *stubCredTypes) List(context.Context) ([]*credential.CredentialType, error) {
	return nil, s.listErr
}

// Delete reports the configured delete failure.
func (s *stubCredTypes) Delete(context.Context, string) error { return s.deleteErr }

// TestPolicyHandlerRefusals pins every refusal on the approval policy endpoints. A policy decides
// which runs are held for a person's sign-off and which are denied outright, so a malformed rule
// that is stored anyway is a gate that silently stops matching, and a store failure reported as a
// success is a rule an operator believes is in force when it is not.
//
// The read-only case matters most: a file-backed policy set refuses writes on purpose, and that has
// to read as a conflict with an explanation rather than as something broken.
//
//nolint:funlen // Test function.
func TestPolicyHandlerRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Method     string
		Path       string
		Body       string
		Store      policy.Store
		Disabled   bool
		WantStatus int
	}{{ // Test 0: Policies are not configured, so creating one says so.
		Name: "create disabled", Method: http.MethodPost, Path: "/v1/policies",
		Body: `{"name":"hold"}`, Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 1: A truncated body is a bad request.
		Name: "create malformed body", Method: http.MethodPost, Path: "/v1/policies",
		Body: `{"name":"hold"`, Store: policy.NewMemStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 2: An undeclared field is refused. A misspelled criterion would otherwise be dropped
		// and the policy stored as a blanket rule matching everything.
		Name: "create unknown field", Method: http.MethodPost, Path: "/v1/policies",
		Body: `{"name":"hold","comand_contains":"rm"}`, Store: policy.NewMemStore(),
		WantStatus: http.StatusBadRequest,
	}, { // Test 3: A policy with no name is refused.
		Name: "create empty name", Method: http.MethodPost, Path: "/v1/policies",
		Body: `{"name":""}`, Store: policy.NewMemStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 4: A tool that is not a supported executor is refused, so a policy cannot be scoped
		// to a tool no run will ever carry.
		Name: "create bad tool", Method: http.MethodPost, Path: "/v1/policies",
		Body: `{"name":"hold","tool":"puppet"}`, Store: policy.NewMemStore(),
		WantStatus: http.StatusBadRequest,
	}, { // Test 5: An effect that is neither require_approval nor deny is refused by validation.
		Name: "create bad effect", Method: http.MethodPost, Path: "/v1/policies",
		Body: `{"name":"hold","effect":"warn"}`, Store: policy.NewMemStore(),
		WantStatus: http.StatusBadRequest,
	}, { // Test 6: An actor kind outside agent and human is refused.
		Name: "create bad actor kind", Method: http.MethodPost, Path: "/v1/policies",
		Body: `{"name":"hold","actor_kind":"robot"}`, Store: policy.NewMemStore(),
		WantStatus: http.StatusBadRequest,
	}, { // Test 7: A risk floor outside low, medium, and high is refused.
		Name: "create bad min risk", Method: http.MethodPost, Path: "/v1/policies",
		Body: `{"name":"hold","min_risk":"extreme"}`, Store: policy.NewMemStore(),
		WantStatus: http.StatusBadRequest,
	}, { // Test 8: A deny policy that also sets a destroy threshold is contradictory, because a
		// denied run is never planned, so it is refused rather than stored as a rule that cannot fire.
		Name: "create deny with max destroy", Method: http.MethodPost, Path: "/v1/policies",
		Body: `{"name":"hold","effect":"deny","max_destroy":3}`, Store: policy.NewMemStore(),
		WantStatus: http.StatusBadRequest,
	}, { // Test 9: The license count is read from the store, so a store that cannot be listed is a
		// server error before anything is written.
		Name: "create list fails", Method: http.MethodPost, Path: "/v1/policies",
		Body: `{"name":"hold"}`, Store: &stubPolicies{listErr: errStore},
		WantStatus: http.StatusInternalServerError,
	}, { // Test 10: An unreachable store on save is a server error.
		Name: "create save fails", Method: http.MethodPost, Path: "/v1/policies",
		Body: `{"name":"hold"}`, Store: &stubPolicies{saveErr: errStore},
		WantStatus: http.StatusInternalServerError,
	}, { // Test 11: A policy source that refuses writes answers a conflict, not a server error, so
		// an operator reads "configured this way" rather than "something broke".
		Name: "create read-only source", Method: http.MethodPost, Path: "/v1/policies",
		Body: `{"name":"hold"}`, Store: &stubPolicies{saveErr: policy.ErrReadOnly},
		WantStatus: http.StatusConflict,
	}, { // Test 12: Updating without policies configured says so.
		Name: "update disabled", Method: http.MethodPut, Path: "/v1/policies/pol_1",
		Body: `{"name":"hold"}`, Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 13: A malformed update body is a bad request.
		Name: "update malformed body", Method: http.MethodPut, Path: "/v1/policies/pol_1",
		Body: `{{`, Store: policy.NewMemStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 14: An update with no name is refused.
		Name: "update empty name", Method: http.MethodPut, Path: "/v1/policies/pol_1",
		Body: `{"name":""}`, Store: policy.NewMemStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 15: An update naming an unsupported tool is refused.
		Name: "update bad tool", Method: http.MethodPut, Path: "/v1/policies/pol_1",
		Body: `{"name":"hold","tool":"chef"}`, Store: policy.NewMemStore(),
		WantStatus: http.StatusBadRequest,
	}, { // Test 16: Updating a policy that does not exist is a not found.
		Name: "update missing", Method: http.MethodPut, Path: "/v1/policies/pol_missing",
		Body: `{"name":"hold"}`, Store: policy.NewMemStore(), WantStatus: http.StatusNotFound,
	}, { // Test 17: A read failure that is not a missing policy is a server error.
		Name: "update read fails", Method: http.MethodPut, Path: "/v1/policies/pol_1",
		Body: `{"name":"hold"}`, Store: &stubPolicies{getErr: errStore},
		WantStatus: http.StatusInternalServerError,
	}, { // Test 18: An unreachable store on the update write is a server error.
		Name: "update save fails", Method: http.MethodPut, Path: "/v1/policies/pol_1",
		Body:       `{"name":"hold"}`,
		Store:      &stubPolicies{present: &policy.Policy{ID: "pol_1"}, saveErr: errStore},
		WantStatus: http.StatusInternalServerError,
	}, { // Test 19: A read-only source refuses the update as a conflict.
		Name: "update read-only source", Method: http.MethodPut, Path: "/v1/policies/pol_1",
		Body:       `{"name":"hold"}`,
		Store:      &stubPolicies{present: &policy.Policy{ID: "pol_1"}, saveErr: policy.ErrReadOnly},
		WantStatus: http.StatusConflict,
	}, { // Test 20: An update that would store an invalid effect is refused.
		Name: "update bad effect", Method: http.MethodPut, Path: "/v1/policies/pol_1",
		Body:       `{"name":"hold","effect":"maybe"}`,
		Store:      &stubPolicies{present: &policy.Policy{ID: "pol_1"}},
		WantStatus: http.StatusBadRequest,
	}, { // Test 21: Listing without policies configured says so.
		Name: "list disabled", Method: http.MethodGet, Path: "/v1/policies",
		Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 22: An unreachable store on list is a server error.
		Name: "list fails", Method: http.MethodGet, Path: "/v1/policies",
		Store: &stubPolicies{listErr: errStore}, WantStatus: http.StatusInternalServerError,
	}, { // Test 23: Deleting without policies configured says so.
		Name: "delete disabled", Method: http.MethodDelete, Path: "/v1/policies/pol_1",
		Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 24: Deleting a policy that does not exist is a not found.
		Name: "delete missing", Method: http.MethodDelete, Path: "/v1/policies/pol_missing",
		Store: policy.NewMemStore(), WantStatus: http.StatusNotFound,
	}, { // Test 25: An unreachable store on delete is a server error, so a gate that is still in
		// force is never reported as removed.
		Name: "delete fails", Method: http.MethodDelete, Path: "/v1/policies/pol_1",
		Store: &stubPolicies{deleteErr: errStore}, WantStatus: http.StatusInternalServerError,
	}, { // Test 26: A read-only source refuses the delete as a conflict.
		Name: "delete read-only source", Method: http.MethodDelete, Path: "/v1/policies/pol_1",
		Store: &stubPolicies{deleteErr: policy.ErrReadOnly}, WantStatus: http.StatusConflict,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var opts []Option
			if !test.Disabled {
				opts = append(opts, WithPolicies(test.Store))
			}
			rec := serveWith(t, test.Method, test.Path, test.Body, opts...)
			if rec.Code != test.WantStatus {
				t.Errorf("%s: status = %d, want %d (body %q)",
					test.Name, rec.Code, test.WantStatus, rec.Body.String())
			}
		})
	}
}

// TestPolicyUpdateKeepsCreationTime pins that replacing a policy keeps the original creation time
// rather than stamping the edit. The register and the audit reading both order policies by when
// they came into force, so an edit that resets the clock rewrites when a gate started applying.
func TestPolicyUpdateKeepsCreationTime(t *testing.T) {
	t.Parallel()
	store := policy.NewMemStore()
	created := policy.NewPolicy("hold")
	if err := store.Save(context.Background(), created); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	rec := serveWith(t, http.MethodPut, "/v1/policies/"+created.ID,
		`{"name":"hold harder","command_contains":"terraform"}`, WithPolicies(store))
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200 (%q)", rec.Code, rec.Body.String())
	}
	var updated policy.Policy
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode updated policy: %v", err)
	}
	if !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("created_at = %v, want the original %v", updated.CreatedAt, created.CreatedAt)
	}
	if updated.ID != created.ID {
		t.Errorf("id = %q, want the original %q", updated.ID, created.ID)
	}
}

// TestPolicyMaxDestroyStaysDisabledWithoutTheField pins the default the doc comment insists on: a
// policy created or updated without max_destroy leaves the plan-content check off. Defaulting it to
// zero instead would hold every terraform apply that destroys anything at all, which is a gate
// nobody asked for appearing on its own.
func TestPolicyMaxDestroyStaysDisabledWithoutTheField(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name           string
		Body           string
		WantMaxDestroy int
	}{{ // Test 0: The field is absent, so the check stays off.
		Name: "absent", Body: `{"name":"hold"}`, WantMaxDestroy: policy.DisabledMaxDestroy,
	}, { // Test 1: An explicit zero means hold on any destroy at all, which is not the same as off.
		Name: "explicit zero", Body: `{"name":"hold","max_destroy":0}`, WantMaxDestroy: 0,
	}, { // Test 2: An explicit negative value disables the check the same way absence does.
		Name: "explicit negative", Body: `{"name":"hold","max_destroy":-5}`, WantMaxDestroy: -5,
	}, { // Test 3: A positive threshold is carried through untouched.
		Name: "positive", Body: `{"name":"hold","max_destroy":25}`, WantMaxDestroy: 25,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec := serveWith(t, http.MethodPost, "/v1/policies", test.Body,
				WithPolicies(policy.NewMemStore()))
			if rec.Code != http.StatusCreated {
				t.Fatalf("%s: status = %d, want 201 (%q)", test.Name, rec.Code, rec.Body.String())
			}
			var got policy.Policy
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode policy: %v", err)
			}
			if diff := cmp.Diff(test.WantMaxDestroy, got.MaxDestroy); diff != "" {
				t.Errorf("%s: max_destroy mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestCredTypeHandlerRefusals pins every refusal on the credential type endpoints. A type defines
// how a credential's fields are injected into a run's environment, so a definition that references
// a field it never declares expands to an empty string and authenticates a run with half a secret.
// Refusing that at definition time is the whole point of validating here.
//
//nolint:funlen // Test function.
func TestCredTypeHandlerRefusals(t *testing.T) {
	t.Parallel()
	valid := `{"name":"Datadog","fields":[{"name":"api_key"}],"env":{"DD_API_KEY":"{{api_key}}"}}`
	tests := []struct {
		Name       string
		Method     string
		Path       string
		Body       string
		Store      credential.TypeStore
		Disabled   bool
		WantStatus int
	}{{ // Test 0: Credential types are not configured, so creating one says so.
		Name: "create disabled", Method: http.MethodPost, Path: "/v1/credential-types",
		Body: valid, Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 1: A malformed body is a bad request.
		Name: "create malformed body", Method: http.MethodPost, Path: "/v1/credential-types",
		Body: `{"name":`, Store: credential.NewMemTypeStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 2: An undeclared field is refused rather than dropped.
		Name: "create unknown field", Method: http.MethodPost, Path: "/v1/credential-types",
		Body:  `{"name":"D","fields":[{"name":"k"}],"env":{"K":"{{k}}"},"secret":true}`,
		Store: credential.NewMemTypeStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 3: A type with no name is refused.
		Name: "create no name", Method: http.MethodPost, Path: "/v1/credential-types",
		Body:  `{"fields":[{"name":"k"}],"env":{"K":"{{k}}"}}`,
		Store: credential.NewMemTypeStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 4: A type with no fields collects nothing, so it is refused.
		Name: "create no fields", Method: http.MethodPost, Path: "/v1/credential-types",
		Body: `{"name":"D","env":{"K":"v"}}`, Store: credential.NewMemTypeStore(),
		WantStatus: http.StatusBadRequest,
	}, { // Test 5: A type that injects nothing does nothing, so it is refused.
		Name: "create no injectors", Method: http.MethodPost, Path: "/v1/credential-types",
		Body: `{"name":"D","fields":[{"name":"k"}]}`, Store: credential.NewMemTypeStore(),
		WantStatus: http.StatusBadRequest,
	}, { // Test 6: A template referencing a field the type does not declare would expand to nothing
		// at run time, so it is refused at definition time instead.
		Name: "create dangling reference", Method: http.MethodPost, Path: "/v1/credential-types",
		Body:  `{"name":"D","fields":[{"name":"k"}],"env":{"K":"{{missing}}"}}`,
		Store: credential.NewMemTypeStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 7: A field declared twice is refused.
		Name: "create duplicate field", Method: http.MethodPost, Path: "/v1/credential-types",
		Body:  `{"name":"D","fields":[{"name":"k"},{"name":"k"}],"env":{"K":"{{k}}"}}`,
		Store: credential.NewMemTypeStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 8: An environment variable name that is not a legal name is refused.
		Name: "create bad env name", Method: http.MethodPost, Path: "/v1/credential-types",
		Body:  `{"name":"D","fields":[{"name":"k"}],"env":{"not a name":"{{k}}"}}`,
		Store: credential.NewMemTypeStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 9: An unreachable store on save is a server error.
		Name: "create store fails", Method: http.MethodPost, Path: "/v1/credential-types",
		Body: valid, Store: &stubCredTypes{saveErr: errStore},
		WantStatus: http.StatusInternalServerError,
	}, { // Test 10: Reading without credential types configured says so.
		Name: "get disabled", Method: http.MethodGet, Path: "/v1/credential-types/ct_1",
		Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 11: Reading a type that does not exist is a not found.
		Name: "get missing", Method: http.MethodGet, Path: "/v1/credential-types/ct_missing",
		Store: credential.NewMemTypeStore(), WantStatus: http.StatusNotFound,
	}, { // Test 12: A read failure that is not a missing type is a server error.
		Name: "get store fails", Method: http.MethodGet, Path: "/v1/credential-types/ct_1",
		Store: &stubCredTypes{getErr: errStore}, WantStatus: http.StatusInternalServerError,
	}, { // Test 13: Listing without credential types configured says so.
		Name: "list disabled", Method: http.MethodGet, Path: "/v1/credential-types",
		Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 14: An unreachable store on list is a server error.
		Name: "list store fails", Method: http.MethodGet, Path: "/v1/credential-types",
		Store: &stubCredTypes{listErr: errStore}, WantStatus: http.StatusInternalServerError,
	}, { // Test 15: Updating without credential types configured says so.
		Name: "update disabled", Method: http.MethodPut, Path: "/v1/credential-types/ct_1",
		Body: valid, Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 16: A malformed update body is a bad request, checked before the type is read.
		Name: "update malformed body", Method: http.MethodPut, Path: "/v1/credential-types/ct_1",
		Body: `nope`, Store: credential.NewMemTypeStore(), WantStatus: http.StatusBadRequest,
	}, { // Test 17: Updating a type that does not exist is a not found.
		Name: "update missing", Method: http.MethodPut, Path: "/v1/credential-types/ct_missing",
		Body: valid, Store: credential.NewMemTypeStore(), WantStatus: http.StatusNotFound,
	}, { // Test 18: A read failure that is not a missing type is a server error.
		Name: "update read fails", Method: http.MethodPut, Path: "/v1/credential-types/ct_1",
		Body: valid, Store: &stubCredTypes{getErr: errStore},
		WantStatus: http.StatusInternalServerError,
	}, { // Test 19: An update that would store a dangling reference is refused, the same as a create.
		Name: "update dangling reference", Method: http.MethodPut,
		Path: "/v1/credential-types/ct_1",
		Body: `{"name":"D","fields":[{"name":"k"}],"env":{"K":"{{gone}}"}}`,
		Store: &stubCredTypes{
			present: &credential.CredentialType{ID: "ct_1", Name: "D"},
		},
		WantStatus: http.StatusBadRequest,
	}, { // Test 20: An unreachable store on the update write is a server error.
		Name: "update save fails", Method: http.MethodPut, Path: "/v1/credential-types/ct_1",
		Body: valid,
		Store: &stubCredTypes{
			present: &credential.CredentialType{ID: "ct_1", Name: "D"}, saveErr: errStore,
		},
		WantStatus: http.StatusInternalServerError,
	}, { // Test 21: Deleting without credential types configured says so.
		Name: "delete disabled", Method: http.MethodDelete, Path: "/v1/credential-types/ct_1",
		Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 22: Deleting a type that does not exist is a not found.
		Name: "delete missing", Method: http.MethodDelete, Path: "/v1/credential-types/ct_missing",
		Store: credential.NewMemTypeStore(), WantStatus: http.StatusNotFound,
	}, { // Test 23: An unreachable store on delete is a server error.
		Name: "delete store fails", Method: http.MethodDelete, Path: "/v1/credential-types/ct_1",
		Store: &stubCredTypes{deleteErr: errStore}, WantStatus: http.StatusInternalServerError,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var opts []Option
			if !test.Disabled {
				opts = append(opts, WithCredentialTypes(test.Store))
			}
			rec := serveWith(t, test.Method, test.Path, test.Body, opts...)
			if rec.Code != test.WantStatus {
				t.Errorf("%s: status = %d, want %d (body %q)",
					test.Name, rec.Code, test.WantStatus, rec.Body.String())
			}
		})
	}
}

// TestCredTypeUpdateKeepsThePathID pins that an update stores under the id in the path and ignores
// any id in the body. Taking the body's id would let one edit overwrite a different type, which is
// how a credential silently starts injecting somebody else's fields.
func TestCredTypeUpdateKeepsThePathID(t *testing.T) {
	t.Parallel()
	store := credential.NewMemTypeStore()
	seeded := &credential.CredentialType{
		ID: "ct_real", Name: "Datadog",
		Fields:       []credential.Field{{Name: "api_key"}},
		EnvInjectors: map[string]string{"DD_API_KEY": "{{api_key}}"},
	}
	if err := store.Save(context.Background(), seeded); err != nil {
		t.Fatalf("seed credential type: %v", err)
	}
	body := `{"id":"ct_other","name":"Renamed","fields":[{"name":"api_key"}],` +
		`"env":{"DD_API_KEY":"{{api_key}}"}}`
	rec := serveWith(t, http.MethodPut, "/v1/credential-types/ct_real", body,
		WithCredentialTypes(store))
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200 (%q)", rec.Code, rec.Body.String())
	}
	var updated credential.CredentialType
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode updated type: %v", err)
	}
	if updated.ID != "ct_real" {
		t.Errorf("id = %q, want the path id ct_real", updated.ID)
	}
	if _, err := store.Get(context.Background(), "ct_other"); err == nil {
		t.Error("the body's id was written as a second type")
	}
	stored, err := store.Get(context.Background(), "ct_real")
	if err != nil {
		t.Fatalf("read back type: %v", err)
	}
	if stored.Name != "Renamed" {
		t.Errorf("stored name = %q, want Renamed", stored.Name)
	}
}

// TestCredTypeCreateIgnoresACallerSuppliedID pins that the server mints the id for a new credential
// type and never takes one from the body. A caller that could choose the id could overwrite an
// existing type through the create route, which has no not-found check to stop it.
func TestCredTypeCreateIgnoresACallerSuppliedID(t *testing.T) {
	t.Parallel()
	store := credential.NewMemTypeStore()
	seeded := &credential.CredentialType{
		ID: "ct_victim", Name: "Original",
		Fields:       []credential.Field{{Name: "api_key"}},
		EnvInjectors: map[string]string{"DD_API_KEY": "{{api_key}}"},
	}
	if err := store.Save(context.Background(), seeded); err != nil {
		t.Fatalf("seed credential type: %v", err)
	}
	body := `{"id":"ct_victim","name":"Impostor","fields":[{"name":"api_key"}],` +
		`"env":{"DD_API_KEY":"{{api_key}}"}}`
	rec := serveWith(t, http.MethodPost, "/v1/credential-types", body,
		WithCredentialTypes(store))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (%q)", rec.Code, rec.Body.String())
	}
	var created credential.CredentialType
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created type: %v", err)
	}
	if created.ID == "ct_victim" {
		t.Fatal("create took the caller's id and overwrote an existing credential type")
	}
	if !strings.HasPrefix(created.ID, "ct") && !strings.Contains(created.ID, "_") {
		t.Errorf("minted id = %q, want a server-generated identifier", created.ID)
	}
	survivor, err := store.Get(context.Background(), "ct_victim")
	if err != nil {
		t.Fatalf("read back seeded type: %v", err)
	}
	if survivor.Name != "Original" {
		t.Errorf("seeded type name = %q, want it untouched as Original", survivor.Name)
	}
}
