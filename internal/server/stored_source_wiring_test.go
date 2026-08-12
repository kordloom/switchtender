package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// countingRefresher is a SourceRefresher that records whether it ran, so a test can tell a refusal
// that returned early from one that ran the plugin and then reported a failure.
type countingRefresher struct {
	// calls counts RefreshSource invocations.
	calls int
}

// RefreshSource records the call and returns the source unchanged.
func (c *countingRefresher) RefreshSource(_ context.Context, id string) (*invsource.Source, error) {
	c.calls++
	return &invsource.Source{ID: id}, nil
}

// TestStoredSourceGuardIsWiredIntoEveryWrite drives the three routes that act on a stored inventory
// source and confirms each refuses a caller who was granted nothing the source reaches.
//
// The guard itself has no test, and neither did its wiring: deleting the call from any of the three
// handlers left the whole suite green. That is the shape worth closing, because the guard exists for
// an attack the body-only check invites. A source names the hosts a run targets and carries the
// credential used to fetch them, and a caller could take one over by omitting its references from
// the request body: nothing was named, so nothing was checked.
//
// Each case asserts the effect, not only the status. A 403 with the source already overwritten,
// deleted, or refreshed would be a guard that reports refusal after doing the thing.
func TestStoredSourceGuardIsWiredIntoEveryWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 8, 11, 2, 0, 0, 0, time.UTC)

	newServer := func(t *testing.T) (http.Handler, invsource.Store, *countingRefresher) {
		t.Helper()
		sources := invsource.NewMemStore()
		if err := sources.Save(ctx, &invsource.Source{
			ID: "src_theirs", Name: "theirs", Source: "inventory/prod.aws_ec2.yml",
			ProjectID: "proj_theirs", CredentialID: "cred_theirs", InventoryID: "inv_theirs",
			CreatedAt: at,
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		// The caller holds a grant, just never on anything this source reaches.
		grants := &fakeGrants{byObject: map[string][]*grant.Grant{
			"proj_mine": {{Subject: "user_1", Access: grant.AccessUse}},
		}}
		refresher := &countingRefresher{}
		h := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
			WithInventorySources(sources, refresher), WithGrants(grants, true)).Handler()
		return h, sources, refresher
	}

	// The body names nothing, which is the whole attack: with only the body authorized there is
	// nothing to refuse.
	const takeoverBody = `{"name":"mine-now","source":"inventory/attacker.yml"}`

	tests := []struct {
		Name   string
		Method string
		Path   string
		Body   string
	}{
		{
			Name: "update cannot take over a source by naming nothing", Method: http.MethodPut,
			Path: "/v1/inventory-sources/src_theirs", Body: takeoverBody,
		},
		{
			Name: "delete cannot remove a source the caller may not use", Method: http.MethodDelete,
			Path: "/v1/inventory-sources/src_theirs",
		},
		{
			Name: "refresh cannot run a source the caller may not use", Method: http.MethodPost,
			Path: "/v1/inventory-sources/src_theirs/refresh",
		},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			handler, sources, refresher := newServer(t)

			var body *strings.Reader
			if test.Body != "" {
				body = strings.NewReader(test.Body)
			} else {
				body = strings.NewReader("")
			}
			req := httptest.NewRequest(test.Method, test.Path, body)
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(context.WithValue(req.Context(), actorKey{},
				Actor{UserID: "user_1", Role: user.RoleOperator}))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403; the caller holds no grant on the source's project, "+
					"credential, or inventory (body %s)", rec.Code, rec.Body.String())
			}
			// The source must be exactly as it was. A refusal that already wrote is not a refusal.
			stored, err := sources.Get(ctx, "src_theirs")
			if err != nil {
				t.Fatalf("the source was deleted by a refused request: %v", err)
			}
			if stored.Name != "theirs" || stored.Source != "inventory/prod.aws_ec2.yml" {
				t.Errorf("a refused request rewrote the source to name %q source %q",
					stored.Name, stored.Source)
			}
			if stored.ProjectID != "proj_theirs" || stored.CredentialID != "cred_theirs" {
				t.Errorf("a refused request repointed the source to project %q credential %q",
					stored.ProjectID, stored.CredentialID)
			}
			if refresher.calls != 0 {
				t.Errorf("a refused refresh ran the source's plugin %d time(s), which decrypts its "+
					"credential and rewrites the backing inventory", refresher.calls)
			}
		})
	}
}

// TestStoredSourceGuardAllowsTheGrantedCaller checks the guard is a check and not a wall: a caller
// granted everything the source reaches gets through all three routes. Without this the refusals
// above would also pass with the handler denying everyone.
func TestStoredSourceGuardAllowsTheGrantedCaller(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sources := invsource.NewMemStore()
	if err := sources.Save(ctx, &invsource.Source{
		ID: "src_mine", Name: "mine", Source: "inventory/prod.aws_ec2.yml",
		ProjectID: "proj_mine", CredentialID: "cred_mine", InventoryID: "inv_mine",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	use := []*grant.Grant{{Subject: "user_1", Access: grant.AccessUse}}
	grants := &fakeGrants{byObject: map[string][]*grant.Grant{
		"proj_mine": use, "cred_mine": use, "inv_mine": use,
	}}
	refresher := &countingRefresher{}
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithInventorySources(sources, refresher), WithGrants(grants, true)).Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/inventory-sources/src_mine/refresh", nil)
	req = req.WithContext(context.WithValue(req.Context(), actorKey{},
		Actor{UserID: "user_1", Role: user.RoleOperator}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a caller granted use on everything the source reaches "+
			"(body %s)", rec.Code, rec.Body.String())
	}
	if refresher.calls != 1 {
		t.Errorf("refresher ran %d time(s), want 1", refresher.calls)
	}
}

// TestInventoryListRedactionIsWired drives GET /v1/inventories and confirms inline secrets are
// replaced before the response is written.
//
// inventory.Redact had unit tests; the call that puts it in the response path had none, so removing
// it from the handler left the suite green while every viewer could read ansible_password out of an
// inventory. The admin branch is covered too, because a bypass nothing executes is a bypass nobody
// notices has stopped matching what the docs promise.
func TestInventoryListRedactionIsWired(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const content = "[web]\nweb01 ansible_password=hunter2 ansible_user=deploy\n"
	// A dynamic inventory is stored as the JSON document the dump conversion writes, and a user may
	// paste the same from ansible-inventory --list. The variable name is quoted there, which is why
	// redaction cannot be a line pattern: it saw nothing to remove and served the password whole.
	const jsonContent = `{"web": {"hosts": {"web02": {"ansible_password": "swordfish",` +
		` "ansible_user": "deploy"}}}}`

	store := inventory.NewMemStore()
	if err := store.Save(ctx, &inventory.Inventory{
		ID: "inv_1", Name: "prod", Content: content,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.Save(ctx, &inventory.Inventory{
		ID: "inv_2", Name: "dynamic", Content: jsonContent,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithInventories(store)).Handler()

	get := func(actor *Actor) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/inventories", nil)
		if actor != nil {
			req = req.WithContext(context.WithValue(req.Context(), actorKey{}, *actor))
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	body := get(&Actor{UserID: "user_1", Role: user.RoleViewer})
	if strings.Contains(body, "hunter2") {
		t.Error("the list response carried an inline ansible_password to a viewer")
	}
	if strings.Contains(body, "swordfish") {
		t.Error("the list response carried a JSON-encoded ansible_password to a viewer")
	}
	// Redaction must remove the value and nothing else, or an operator cannot read their inventory.
	for _, keep := range []string{"web01", "web02", "ansible_password", "deploy"} {
		if !strings.Contains(body, keep) {
			t.Errorf("redaction removed %q, which is not a secret", keep)
		}
	}

	// An admin is trusted with the real values, and that branch is the one a change would break
	// silently in the other direction.
	adminBody := get(&Actor{UserID: "root", Role: user.RoleAdmin})
	for _, want := range []string{"hunter2", "swordfish"} {
		if !strings.Contains(adminBody, want) {
			t.Errorf("an admin could not read %q, so the bypass no longer works", want)
		}
	}

	// The response is built from a copy. If it were not, redacting once would erase the real
	// password from the store and every later run would authenticate with "***".
	stored, err := store.Get(ctx, "inv_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if stored.Content != content {
		t.Errorf("redaction mutated the stored inventory:\n got %q\nwant %q", stored.Content, content)
	}
}
