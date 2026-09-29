package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/template"
)

// credRefStores holds the four configuration stores a credential can be referenced from, so a test
// can attach one reference and check the delete guard sees it.
type credRefStores struct {
	// templates hold credential references in several distinct fields.
	templates template.Store
	// inventories hold a list of credentials materialized for every run against them.
	inventories inventory.Store
	// projects hold a checkout credential and a registry pull credential.
	projects project.Store
	// invSources hold the credential their inventory plugin authenticates with.
	invSources invsource.Store
}

// newCredRefStores returns empty stores plus the refChecker over them.
func newCredRefStores() (*credRefStores, *refChecker) {
	s := &credRefStores{
		templates:   template.NewMemStore(),
		inventories: inventory.NewMemStore(),
		projects:    project.NewMemStore(),
		invSources:  invsource.NewMemStore(),
	}
	return s, &refChecker{
		templates:   s.templates,
		inventories: s.inventories,
		projects:    s.projects,
		invSources:  s.invSources,
	}
}

// deleteCredential runs the delete handler against a store holding the credential and returns the
// response, which is what an operator clicking delete in the UI actually gets.
func deleteCredential(t *testing.T, refs *refChecker, id string) *httptest.ResponseRecorder {
	t.Helper()
	store := credential.NewMemStore()
	if err := store.Save(context.Background(), &credential.Credential{
		ID: id, Name: "the credential", Kind: credential.KindEnv,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	h := deleteCredentialHandler(store, refs, zap.NewNop())
	req := httptest.NewRequest(http.MethodDelete, "/v1/credentials/"+id, nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// TestDeletingACredentialIsRefusedWhileAnythingUsesIt walks every field a credential can be
// referenced from and checks the delete guard sees each one.
//
// The guard is the only thing standing between a delete and a fleet-wide breakage: a credential
// removed while a template, inventory, project, or inventory source still names it does not fail
// loudly, it fails at the next launch, on real hosts, with an authentication error nobody connects
// back to the delete. Only one of the four branches was executed by any test, so the other three
// were free to be wrong.
func TestDeletingACredentialIsRefusedWhileAnythingUsesIt(t *testing.T) {
	t.Parallel()
	const credID = "cred_1"
	tests := []struct {
		Name     string
		Attach   func(t *testing.T, s *credRefStores)
		WantKind string
	}{
		{
			Name: "a template materializes it on every launch",
			Attach: func(t *testing.T, s *credRefStores) {
				t.Helper()
				save(t, s.templates.Save, &template.Template{
					ID: "tpl_1", Name: "deploy", CredentialIDs: []string{credID},
				})
			},
			WantKind: "templates",
		},
		{
			Name: "an inventory materializes it for every run against it",
			Attach: func(t *testing.T, s *credRefStores) {
				t.Helper()
				save(t, s.inventories.Save, &inventory.Inventory{
					ID: "inv_1", Name: "prod", CredentialIDs: []string{credID},
				})
			},
			WantKind: "inventories",
		},
		{
			Name: "a project checks its repository out with it",
			Attach: func(t *testing.T, s *credRefStores) {
				t.Helper()
				save(t, s.projects.Save, &project.Project{
					ID: "prj_1", Name: "playbooks", CredentialID: credID,
				})
			},
			WantKind: "projects",
		},
		{
			Name: "a project pulls its private image with it",
			Attach: func(t *testing.T, s *credRefStores) {
				t.Helper()
				save(t, s.projects.Save, &project.Project{
					ID: "prj_1", Name: "playbooks", PullCredentialID: credID,
				})
			},
			WantKind: "projects",
		},
		{
			Name: "an inventory source authenticates its plugin with it",
			Attach: func(t *testing.T, s *credRefStores) {
				t.Helper()
				save(t, s.invSources.Save, &invsource.Source{
					ID: "src_1", Name: "aws-ec2", CredentialID: credID,
				})
			},
			WantKind: "inventory_sources",
		},
		{
			Name: "a template pulls its private image with it",
			// The project branch already checks its pull credential. A template's execution
			// environment outranks the project's, so this is the same reference on the object that
			// wins, and deleting it breaks every launch of that template at the image pull.
			Attach: func(t *testing.T, s *credRefStores) {
				t.Helper()
				save(t, s.templates.Save, &template.Template{
					ID: "tpl_1", Name: "deploy", Image: "registry.example.com/app:1",
					PullCredentialID: credID,
				})
			},
			WantKind: "templates",
		},
		{
			Name: "a template offers it as a launch-time choice",
			// A launch may pick from this menu and a choice outside it is rejected, so the id is a
			// live reference: deleting it silently removes an option the template promises.
			Attach: func(t *testing.T, s *credRefStores) {
				t.Helper()
				save(t, s.templates.Save, &template.Template{
					ID: "tpl_1", Name: "deploy", SelectableCredentialIDs: []string{credID},
				})
			},
			WantKind: "templates",
		},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			stores, refs := newCredRefStores()
			test.Attach(t, stores)

			rec := deleteCredential(t, refs, credID)

			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409: the credential was deleted while %s still use it",
					rec.Code, test.WantKind)
			}
			var body struct {
				Error  string              `json:"error"`
				UsedBy map[string][]string `json:"used_by"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if len(body.UsedBy[test.WantKind]) == 0 {
				t.Errorf("used_by = %v, want it to name the %s holding the credential",
					body.UsedBy, test.WantKind)
			}
		})
	}
}

// TestDeletingAnUnusedCredentialSucceeds is the other side of the guard: nothing referencing the
// credential has to mean the delete goes through, or every credential becomes undeletable.
func TestDeletingAnUnusedCredentialSucceeds(t *testing.T) {
	t.Parallel()
	stores, refs := newCredRefStores()
	// Objects that reference a different credential must not block this one.
	save(t, stores.templates.Save, &template.Template{
		ID: "tpl_1", Name: "deploy", CredentialIDs: []string{"cred_other"},
	})
	save(t, stores.projects.Save, &project.Project{
		ID: "prj_1", Name: "playbooks", CredentialID: "cred_other",
	})

	rec := deleteCredential(t, refs, "cred_1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestDeletingACredentialFailsClosedWhenTheReferenceCheckCannotRun checks a store that cannot answer
// does not turn into a delete. Failing open here removes a credential the server could not prove was
// unused, which is the one outcome that cannot be undone.
func TestDeletingACredentialFailsClosedWhenTheReferenceCheckCannotRun(t *testing.T) {
	t.Parallel()
	stores, refs := newCredRefStores()
	refs.templates = &failingTemplateStore{Store: stores.templates}

	rec := deleteCredential(t, refs, "cred_1")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 rather than a delete the server could not justify: %s",
			rec.Code, rec.Body.String())
	}
}

// save stores one object through a typed store Save, failing the test on error.
func save[T any](t *testing.T, fn func(context.Context, T) error, v T) {
	t.Helper()
	if err := fn(context.Background(), v); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
}

// failingTemplateStore is a template store whose List always fails, standing in for a database that
// is down while a delete is attempted.
type failingTemplateStore struct {
	// Store is the wrapped store every other call passes through to.
	template.Store
}

// List always fails.
func (f *failingTemplateStore) List(context.Context) ([]*template.Template, error) {
	return nil, errors.New("store is down")
}
