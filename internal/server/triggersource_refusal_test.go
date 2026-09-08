package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
)

// stubTriggers is a trigger.Store that answers Get from a fixed trigger and fails whichever calls a
// test asks it to, so the webhook handlers' error paths are reachable without a database.
type stubTriggers struct {
	// present is what Get answers with, nil to answer trigger.ErrNotFound.
	present *trigger.Trigger
	// getErr, listErr, saveErr, and deleteErr replace the ordinary answer of the method they name.
	getErr    error
	listErr   error
	saveErr   error
	deleteErr error
}

// Save reports the configured save failure.
func (s *stubTriggers) Save(context.Context, *trigger.Trigger) error { return s.saveErr }

// Get answers with the fixed trigger, its configured error, or trigger.ErrNotFound.
func (s *stubTriggers) Get(context.Context, string) (*trigger.Trigger, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.present == nil {
		return nil, trigger.ErrNotFound
	}
	return s.present, nil
}

// List reports the configured list failure.
func (s *stubTriggers) List(context.Context) ([]*trigger.Trigger, error) { return nil, s.listErr }

// Delete reports the configured delete failure.
func (s *stubTriggers) Delete(context.Context, string) error { return s.deleteErr }

// FindByTokenHash answers not found, so no webhook token ever resolves against this store.
func (s *stubTriggers) FindByTokenHash(context.Context, string) (*trigger.Trigger, error) {
	return nil, trigger.ErrNotFound
}

// stubSources is an invsource.Store that answers Get from a fixed source and fails whichever calls a
// test asks it to.
type stubSources struct {
	// present is what Get answers with, nil to answer invsource.ErrNotFound.
	present *invsource.Source
	// getErr, listErr, saveErr, updateErr, and deleteErr replace the ordinary answer of the method
	// they name.
	getErr    error
	listErr   error
	saveErr   error
	updateErr error
	deleteErr error
}

// Save reports the configured save failure.
func (s *stubSources) Save(context.Context, *invsource.Source) error { return s.saveErr }

// Update reports the configured update failure.
func (s *stubSources) Update(context.Context, *invsource.Source) error { return s.updateErr }

// Get answers with the fixed source, its configured error, or invsource.ErrNotFound.
func (s *stubSources) Get(context.Context, string) (*invsource.Source, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.present == nil {
		return nil, invsource.ErrNotFound
	}
	return s.present, nil
}

// List reports the configured list failure.
func (s *stubSources) List(context.Context) ([]*invsource.Source, error) { return nil, s.listErr }

// Delete reports the configured delete failure.
func (s *stubSources) Delete(context.Context, string) error { return s.deleteErr }

// stubRefresher runs a source and returns canned results.
type stubRefresher struct {
	// result is returned on success.
	result *invsource.Source
	// err is returned instead of result when non-nil.
	err error
}

// RefreshSource returns the configured source or error.
func (s *stubRefresher) RefreshSource(context.Context, string) (*invsource.Source, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.result, nil
}

// TestTriggerHandlerRefusals pins the refusals on the webhook trigger endpoints. A trigger is a
// bearer credential in a URL that launches a template against real hosts, so a create that stores
// one for a template that does not exist, or a delete reported as done when it failed, both leave a
// live launch point nobody accounted for.
//
//nolint:funlen // Test function.
func TestTriggerHandlerRefusals(t *testing.T) {
	t.Parallel()
	existing := &trigger.Trigger{ID: "trg_1", Name: "deploy", TemplateID: "tpl_1"}
	tests := []struct {
		Name       string
		Method     string
		Path       string
		Body       string
		Store      trigger.Store
		Templates  template.Store
		Disabled   bool
		WantStatus int
	}{{ // Test 0: Triggers are not configured, so creating one says so.
		Name: "create disabled", Method: http.MethodPost, Path: "/v1/triggers",
		Body: `{"name":"deploy","template_id":"tpl_1"}`, Disabled: true,
		WantStatus: http.StatusNotFound,
	}, { // Test 1: A malformed body is a bad request.
		Name: "create malformed body", Method: http.MethodPost, Path: "/v1/triggers",
		Body: `{"name":`, Store: &stubTriggers{}, WantStatus: http.StatusBadRequest,
	}, { // Test 2: An undeclared field is refused. A misspelled require_signature would otherwise be
		// dropped and the trigger stored accepting unsigned deliveries.
		Name: "create unknown field", Method: http.MethodPost, Path: "/v1/triggers",
		Body:  `{"name":"deploy","template_id":"tpl_1","require_signatures":true}`,
		Store: &stubTriggers{}, WantStatus: http.StatusBadRequest,
	}, { // Test 3: A trigger with no name is refused, since a webhook nobody can identify is one
		// nobody will ever revoke.
		Name: "create no name", Method: http.MethodPost, Path: "/v1/triggers",
		Body: `{"template_id":"tpl_1"}`, Store: &stubTriggers{},
		WantStatus: http.StatusBadRequest,
	}, { // Test 4: A trigger naming no template fires nothing, so it is refused.
		Name: "create no template", Method: http.MethodPost, Path: "/v1/triggers",
		Body: `{"name":"deploy"}`, Store: &stubTriggers{}, WantStatus: http.StatusBadRequest,
	}, { // Test 5: Listing without triggers configured says so.
		Name: "list disabled", Method: http.MethodGet, Path: "/v1/triggers",
		Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 6: An unreachable store on list is a server error.
		Name: "list store fails", Method: http.MethodGet, Path: "/v1/triggers",
		Store: &stubTriggers{listErr: errStore}, WantStatus: http.StatusInternalServerError,
	}, { // Test 7: Deleting without triggers configured says so.
		Name: "delete disabled", Method: http.MethodDelete, Path: "/v1/triggers/trg_1",
		Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 8: Deleting a trigger that does not exist is a not found, decided on the read that
		// the template authorization needs anyway.
		Name: "delete missing", Method: http.MethodDelete, Path: "/v1/triggers/trg_missing",
		Store: &stubTriggers{}, WantStatus: http.StatusNotFound,
	}, { // Test 9: A read failure that is not a missing trigger is a server error.
		Name: "delete read fails", Method: http.MethodDelete, Path: "/v1/triggers/trg_1",
		Store: &stubTriggers{getErr: errStore}, WantStatus: http.StatusInternalServerError,
	}, { // Test 10: An unreachable store on delete is a server error, so a live webhook is never
		// reported as revoked.
		Name: "delete store fails", Method: http.MethodDelete, Path: "/v1/triggers/trg_1",
		Store:      &stubTriggers{present: existing, deleteErr: errStore},
		WantStatus: http.StatusInternalServerError,
	}, { // Test 11: A trigger that vanishes between the read and the delete is still a not found.
		Name: "delete races away", Method: http.MethodDelete, Path: "/v1/triggers/trg_1",
		Store:      &stubTriggers{present: existing, deleteErr: trigger.ErrNotFound},
		WantStatus: http.StatusNotFound,
	}, { // Test 12: Rotating a secret without triggers configured says so.
		Name: "rotate disabled", Method: http.MethodPost,
		Path: "/v1/triggers/trg_1/rotate-secret", Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 13: Updating without triggers configured says so.
		Name: "update disabled", Method: http.MethodPut, Path: "/v1/triggers/trg_1",
		Body: `{"name":"deploy","template_id":"tpl_1"}`, Disabled: true,
		WantStatus: http.StatusNotFound,
	}, { // Test 14: A malformed update body is a bad request.
		Name: "update malformed body", Method: http.MethodPut, Path: "/v1/triggers/trg_1",
		Body: `nope`, Store: &stubTriggers{present: existing}, WantStatus: http.StatusBadRequest,
	}, { // Test 15: The update body declares only a name and the signature toggle, so a template_id
		// in it is refused. That is what stops a trigger being repointed at a different template
		// through the edit route, where the authorization check reads the template it already has.
		Name: "update cannot repoint the template", Method: http.MethodPut,
		Path: "/v1/triggers/trg_1", Body: `{"name":"deploy","template_id":"tpl_other"}`,
		Store: &stubTriggers{present: existing}, WantStatus: http.StatusBadRequest,
	}, { // Test 16: Updating a trigger that does not exist is a not found.
		Name: "update missing", Method: http.MethodPut, Path: "/v1/triggers/trg_missing",
		Body: `{"name":"deploy"}`, Store: &stubTriggers{}, WantStatus: http.StatusNotFound,
	}, { // Test 17: An update with no name is refused before the trigger is read.
		Name: "update no name", Method: http.MethodPut, Path: "/v1/triggers/trg_1",
		Body: `{"name":""}`, Store: &stubTriggers{present: existing},
		WantStatus: http.StatusBadRequest,
	}, { // Test 18: Turning signature enforcement on when the trigger has no signing secret is
		// refused, so a trigger can never be left demanding a signature it has no way to check.
		Name: "require signature without a secret", Method: http.MethodPut,
		Path: "/v1/triggers/trg_1", Body: `{"name":"deploy","require_signature":true}`,
		Store: &stubTriggers{present: existing}, WantStatus: http.StatusConflict,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			opts := []Option{WithTemplates(template.NewMemStore())}
			if !test.Disabled {
				opts = append(opts, WithTriggers(test.Store, nil))
			}
			rec := serveWith(t, test.Method, test.Path, test.Body, opts...)
			if rec.Code != test.WantStatus {
				t.Errorf("%s: status = %d, want %d (body %q)",
					test.Name, rec.Code, test.WantStatus, rec.Body.String())
			}
		})
	}
}

// TestTriggerListNeverCarriesTheWebhookToken pins that neither the token nor the signing secret
// leaves the process through the trigger list. The URL token is a bearer credential that launches a
// template, and only its hash is stored, so a list that echoed either would hand every reader a way
// to fire somebody's deployment.
func TestTriggerListNeverCarriesTheWebhookToken(t *testing.T) {
	t.Parallel()
	store := trigger.NewMemStore()
	plain, tg, err := trigger.New("deploy", "tpl_1")
	if err != nil {
		t.Fatalf("mint trigger: %v", err)
	}
	tg.SigningSecret = "sealed-signing-material"
	if err := store.Save(context.Background(), tg); err != nil {
		t.Fatalf("seed trigger: %v", err)
	}
	rec := serveWith(t, http.MethodGet, "/v1/triggers", "", WithTriggers(store, nil),
		WithTemplates(template.NewMemStore()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%q)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, plain) {
		t.Error("the trigger list carries the plaintext webhook token")
	}
	if strings.Contains(body, tg.TokenHash) {
		t.Error("the trigger list carries the webhook token hash")
	}
	if strings.Contains(body, "sealed-signing-material") {
		t.Error("the trigger list carries the sealed signing secret")
	}
	// The list still has to be useful, so the trigger's identity and target are present.
	var listed listTriggersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode trigger list: %v", err)
	}
	if listed.Count != 1 || listed.Triggers[0].TemplateID != "tpl_1" {
		t.Errorf("trigger list = %+v, want one trigger naming tpl_1", listed)
	}
}

// TestInventorySourceHandlerRefusals pins the refusals on the dynamic inventory source endpoints. A
// source decides which hosts a run targets and carries the credential used to fetch them, so a
// handler that acts without reading the stored object, or reports a failed write as a success, moves
// production work or borrows a secret without anybody noticing.
//
//nolint:funlen // Test function.
func TestInventorySourceHandlerRefusals(t *testing.T) {
	t.Parallel()
	stored := &invsource.Source{ID: "src_1", Name: "aws", Source: "aws_ec2.yml"}
	valid := `{"name":"aws","source":"aws_ec2.yml"}`
	tests := []struct {
		Name       string
		Method     string
		Path       string
		Body       string
		Sources    invsource.Store
		Refresher  SourceRefresher
		Disabled   bool
		WantStatus int
	}{{ // Test 0: Inventory sources are not configured, so creating one says so.
		Name: "create disabled", Method: http.MethodPost, Path: "/v1/inventory-sources",
		Body: valid, Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 1: A malformed body is a bad request.
		Name: "create malformed body", Method: http.MethodPost, Path: "/v1/inventory-sources",
		Body: `{"name":`, Sources: &stubSources{}, WantStatus: http.StatusBadRequest,
	}, { // Test 2: An undeclared field is refused rather than dropped, so a misspelled sync interval
		// cannot leave a source refreshing on a schedule nobody asked for.
		Name: "create unknown field", Method: http.MethodPost, Path: "/v1/inventory-sources",
		Body:    `{"name":"aws","source":"aws_ec2.yml","sync_interval":60}`,
		Sources: &stubSources{}, WantStatus: http.StatusBadRequest,
	}, { // Test 3: A source with no name is refused.
		Name: "create no name", Method: http.MethodPost, Path: "/v1/inventory-sources",
		Body: `{"source":"aws_ec2.yml"}`, Sources: &stubSources{},
		WantStatus: http.StatusBadRequest,
	}, { // Test 4: A source naming nothing to run is refused.
		Name: "create no source", Method: http.MethodPost, Path: "/v1/inventory-sources",
		Body: `{"name":"aws"}`, Sources: &stubSources{}, WantStatus: http.StatusBadRequest,
	}, { // Test 5: An unreachable store on save is a server error.
		Name: "create store fails", Method: http.MethodPost, Path: "/v1/inventory-sources",
		Body: valid, Sources: &stubSources{saveErr: errStore},
		WantStatus: http.StatusInternalServerError,
	}, { // Test 6: Listing without sources configured says so.
		Name: "list disabled", Method: http.MethodGet, Path: "/v1/inventory-sources",
		Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 7: An unreachable store on list is a server error.
		Name: "list store fails", Method: http.MethodGet, Path: "/v1/inventory-sources",
		Sources: &stubSources{listErr: errStore}, WantStatus: http.StatusInternalServerError,
	}, { // Test 8: Updating a source that does not exist is a not found.
		Name: "update missing", Method: http.MethodPut, Path: "/v1/inventory-sources/src_missing",
		Body: valid, Sources: &stubSources{}, WantStatus: http.StatusNotFound,
	}, { // Test 9: A read failure that is not a missing source is a server error, because the
		// handler must authorize the stored object before it writes.
		Name: "update read fails", Method: http.MethodPut, Path: "/v1/inventory-sources/src_1",
		Body: valid, Sources: &stubSources{getErr: errStore},
		WantStatus: http.StatusInternalServerError,
	}, { // Test 10: An update body with no name is refused before anything is read.
		Name: "update no name", Method: http.MethodPut, Path: "/v1/inventory-sources/src_1",
		Body: `{"source":"aws_ec2.yml"}`, Sources: &stubSources{present: stored},
		WantStatus: http.StatusBadRequest,
	}, { // Test 11: An unreachable store on the update write is a server error.
		Name: "update store fails", Method: http.MethodPut, Path: "/v1/inventory-sources/src_1",
		Body: valid, Sources: &stubSources{present: stored, updateErr: errStore},
		WantStatus: http.StatusInternalServerError,
	}, { // Test 12: A source that vanishes between the read and the write is a not found.
		Name: "update races away", Method: http.MethodPut, Path: "/v1/inventory-sources/src_1",
		Body:       valid,
		Sources:    &stubSources{present: stored, updateErr: invsource.ErrNotFound},
		WantStatus: http.StatusNotFound,
	}, { // Test 13: Deleting without sources configured says so.
		Name: "delete disabled", Method: http.MethodDelete, Path: "/v1/inventory-sources/src_1",
		Disabled: true, WantStatus: http.StatusNotFound,
	}, { // Test 14: Deleting a source that does not exist is a not found.
		Name: "delete missing", Method: http.MethodDelete,
		Path: "/v1/inventory-sources/src_missing", Sources: &stubSources{},
		WantStatus: http.StatusNotFound,
	}, { // Test 15: An unreachable store on delete is a server error, so a refresh that is still
		// running is never reported as stopped.
		Name: "delete store fails", Method: http.MethodDelete,
		Path:       "/v1/inventory-sources/src_1",
		Sources:    &stubSources{present: stored, deleteErr: errStore},
		WantStatus: http.StatusInternalServerError,
	}, { // Test 16: Refreshing a source that does not exist is a not found.
		Name: "refresh missing", Method: http.MethodPost,
		Path: "/v1/inventory-sources/src_missing/refresh", Sources: &stubSources{},
		Refresher: &stubRefresher{}, WantStatus: http.StatusNotFound,
	}, { // Test 17: A refresh that fails is a bad gateway, and the plugin's own error is kept out of
		// the response because it routinely names cloud accounts and endpoints.
		Name: "refresh fails", Method: http.MethodPost, Path: "/v1/inventory-sources/src_1/refresh",
		Sources:   &stubSources{present: stored},
		Refresher: &stubRefresher{err: errStore}, WantStatus: http.StatusBadGateway,
	}, { // Test 18: A refresh of a source the refresher reports gone is a not found.
		Name: "refresh races away", Method: http.MethodPost,
		Path: "/v1/inventory-sources/src_1/refresh", Sources: &stubSources{present: stored},
		Refresher: &stubRefresher{err: invsource.ErrNotFound}, WantStatus: http.StatusNotFound,
	}, { // Test 19: A successful refresh returns the updated source.
		Name: "refresh succeeds", Method: http.MethodPost,
		Path: "/v1/inventory-sources/src_1/refresh", Sources: &stubSources{present: stored},
		Refresher: &stubRefresher{result: stored}, WantStatus: http.StatusOK,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var opts []Option
			if !test.Disabled {
				opts = append(opts, WithInventorySources(test.Sources, test.Refresher),
					WithInventories(inventory.NewMemStore()))
			}
			rec := serveWith(t, test.Method, test.Path, test.Body, opts...)
			if rec.Code != test.WantStatus {
				t.Errorf("%s: status = %d, want %d (body %q)",
					test.Name, rec.Code, test.WantStatus, rec.Body.String())
			}
		})
	}
}

// TestRefreshFailureNeverEchoesThePluginError pins that a failed inventory refresh answers with a
// generic message. The plugin's output is recorded on the source as its last error, which an admin
// reads back deliberately, and it routinely names cloud accounts, role ARNs, and internal endpoints.
// Returning it from the refresh endpoint would put that in front of every caller who can press the
// button.
func TestRefreshFailureNeverEchoesThePluginError(t *testing.T) {
	t.Parallel()
	leaky := fmt.Errorf("botocore: AccessDenied for arn:aws:iam::123456789012:role/secret-role " +
		"at https://ec2.internal.example")
	stored := &invsource.Source{ID: "src_1", Name: "aws", Source: "aws_ec2.yml"}
	rec := serveWith(t, http.MethodPost, "/v1/inventory-sources/src_1/refresh", "",
		WithInventorySources(&stubSources{present: stored}, &stubRefresher{err: leaky}),
		WithInventories(inventory.NewMemStore()))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (%q)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, secret := range []string{"123456789012", "secret-role", "ec2.internal.example",
		"AccessDenied"} {
		if strings.Contains(body, secret) {
			t.Errorf("the refresh failure echoes %q back to the caller: %s", secret, body)
		}
	}
	if !strings.Contains(body, "inventory refresh failed") {
		t.Errorf("body = %q, want the generic refusal", body)
	}
}
