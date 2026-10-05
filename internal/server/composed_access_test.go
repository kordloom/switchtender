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

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/inventorytest"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// composedAccessFixture is an install under strict grants with two inventories with hosts, a smart
// inventory over both, and a constructed inventory reading both. The operator may use the two
// composed inventories and inv_web, and holds no grant on inv_db.
type composedAccessFixture struct {
	// handler serves the API.
	handler http.Handler
	// runs is the run store the dispatcher writes.
	runs run.Store
	// operator is the operator's bearer token.
	operator string
	// admin is the admin's bearer token.
	admin string
}

// newComposedAccessFixture builds the install, resolving inventories without Ansible.
func newComposedAccessFixture(t *testing.T) *composedAccessFixture {
	t.Helper()
	ctx := context.Background()
	users := user.NewMemStore()
	tokens := auth.NewMemStore()
	grants := grant.NewMemStore()
	invs := inventory.NewMemStore()
	runs := run.NewMemStore()

	bearer := map[user.Role]string{}
	ids := map[user.Role]string{}
	for _, role := range []user.Role{user.RoleOperator, user.RoleAdmin} {
		u, err := user.New(string(role), "pw", role)
		if err != nil {
			t.Fatalf("user.New() error = %v", err)
		}
		if err := users.Save(ctx, u); err != nil {
			t.Fatalf("users.Save() error = %v", err)
		}
		plain, tok, err := auth.New("t-" + string(role))
		if err != nil {
			t.Fatalf("auth.New() error = %v", err)
		}
		tok.UserID = u.ID
		if err := tokens.Save(ctx, tok); err != nil {
			t.Fatalf("tokens.Save() error = %v", err)
		}
		bearer[role], ids[role] = plain, u.ID
	}
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for n, inv := range []*inventory.Inventory{{
		ID: "inv_web", Name: "web fleet",
		Content: inventorytest.Listing(map[string][]string{"web": {"web1", "web2"}}, nil),
	}, {
		ID: "inv_db", Name: "db fleet",
		Content: inventorytest.Listing(map[string][]string{"db": {"db1"}, "web": {"web7"}},
			map[string]map[string]any{"db1": {"ansible_password": "hunter2-secret"}}),
	}, {
		ID: "inv_smart", Name: "everything", Kind: inventory.KindSmart,
		HostFilter: "groups__name=web or groups__name=db",
	}, {
		ID: "inv_built", Name: "built", Kind: inventory.KindConstructed,
		InputIDs: []string{"inv_web", "inv_db"},
	}} {
		inv.CreatedAt = base.Add(time.Duration(n) * time.Minute)
		if err := invs.Save(ctx, inv); err != nil {
			t.Fatalf("Save(%s) error = %v", inv.ID, err)
		}
	}
	for _, g := range []*grant.Grant{
		{Subject: ids[user.RoleOperator], Object: "inv_smart", Access: grant.AccessUse},
		{Subject: ids[user.RoleOperator], Object: "inv_built", Access: grant.AccessManage},
		{Subject: ids[user.RoleOperator], Object: "inv_web", Access: grant.AccessUse},
	} {
		g.ID = grant.NewID()
		if err := grants.Save(ctx, g); err != nil {
			t.Fatalf("grants.Save() error = %v", err)
		}
	}

	disp := dispatch.New(runs, &inventorytest.ListingRunner{}, zap.NewNop(),
		dispatch.WithInventories(invs))
	t.Cleanup(disp.Close)
	handler := New(runs, disp, zap.NewNop(), WithTokens(tokens), WithUsers(users),
		WithGrants(grants, true), WithInventories(invs), WithInventoryPreviewer(disp)).Handler()
	return &composedAccessFixture{
		handler: handler, runs: runs,
		operator: bearer[user.RoleOperator], admin: bearer[user.RoleAdmin],
	}
}

// call sends one request as the bearer and returns the status and body.
func (f *composedAccessFixture) call(t *testing.T, bearer, method, path, body string) (int, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	f.handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// TestComposedInventoryDoesNotWidenAccess pins the access rule for smart and constructed
// inventories end to end. An operator allowed to use a composed inventory, and one of its inputs
// but not the other, previews and launches against only the hosts of the input they may use, and
// cannot add the other as an input. Composing would otherwise be a way to reach every host on the
// install through one grant.
func TestComposedInventoryDoesNotWidenAccess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Admin     bool
		Path      string
		WantHosts []string
	}{{ // Test 0: The smart inventory previews only the hosts of inv_web for the operator.
		Path: "/v1/inventories/inv_smart/preview", WantHosts: []string{"web1", "web2"},
	}, { // Test 1: The constructed inventory previews only the hosts of inv_web for the operator.
		Path: "/v1/inventories/inv_built/preview", WantHosts: []string{"web1", "web2"},
	}, { // Test 2: An admin, who may use every inventory, previews every host.
		Admin: true, Path: "/v1/inventories/inv_smart/preview",
		WantHosts: []string{"db1", "web1", "web2", "web7"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f := newComposedAccessFixture(t)
			bearer := f.operator
			if test.Admin {
				bearer = f.admin
			}
			code, body := f.call(t, bearer, http.MethodPost, test.Path, "")
			if code != http.StatusOK {
				t.Fatalf("preview = %d %s, want 200", code, body)
			}
			var got inventoryPreviewResponse
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("decode preview: %v", err)
			}
			if diff := cmp.Diff(test.WantHosts, got.Hosts, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("previewed hosts mismatch (-want +got):\n%s", diff)
			}
			if strings.Contains(string(body), "hunter2-secret") {
				t.Error("a preview carried a host variable's secret")
			}
		})
	}

	t.Run("a launch records only the hosts the operator may reach", func(t *testing.T) {
		t.Parallel()
		f := newComposedAccessFixture(t)
		code, body := f.call(t, f.operator, http.MethodPost, "/v1/runs",
			`{"tool":"ansible","playbook":"site.yml","inventory_id":"inv_smart"}`)
		if code != http.StatusAccepted {
			t.Fatalf("launch = %d %s, want 202", code, body)
		}
		var created run.Run
		if err := json.Unmarshal(body, &created); err != nil {
			t.Fatalf("decode run: %v", err)
		}
		stored, err := f.runs.Get(context.Background(), created.ID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		want := &run.InventoryResolution{
			Kind: "smart", Inputs: []string{"inv_web"}, Hosts: []string{"web1", "web2"},
			Engine: "native",
		}
		digests := cmpopts.IgnoreFields(run.InventoryResolution{}, "InputDigest", "ResolvedDigest")
		if diff := cmp.Diff(want, stored.InventoryResolution, digests); diff != "" {
			t.Errorf("recorded resolution mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("an input the operator may not use cannot be added", func(t *testing.T) {
		t.Parallel()
		f := newComposedAccessFixture(t)
		code, body := f.call(t, f.operator, http.MethodPut, "/v1/inventories/inv_built",
			`{"name":"built","kind":"constructed","input_inventory_ids":["inv_web","inv_db"]}`)
		if code != http.StatusForbidden {
			t.Errorf("naming inv_db as an input = %d %s, want 403", code, body)
		}
		code, body = f.call(t, f.operator, http.MethodPut, "/v1/inventories/inv_built",
			`{"name":"built","kind":"constructed","input_inventory_ids":["inv_web"]}`)
		if code != http.StatusOK {
			t.Errorf("naming only inv_web = %d %s, want 200", code, body)
		}
		code, body = f.call(t, f.operator, http.MethodPost, "/v1/inventories/preview",
			`{"kind":"constructed","input_inventory_ids":["inv_db"]}`)
		if code != http.StatusForbidden {
			t.Errorf("previewing inv_db through a constructed definition = %d %s, want 403", code, body)
		}
	})
}

// TestComposedInventoryAPIValidates pins that a composed inventory's definition is checked when it
// is saved, not discovered broken at the first launch.
func TestComposedInventoryAPIValidates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Body     string
		WantCode int
	}{
		// Test 0: A smart inventory with a readable filter is stored.
		{Body: `{"name":"s","kind":"smart","host_filter":"groups__name=web"}`,
			WantCode: http.StatusCreated},
		// Test 1: A filter naming an unknown field is refused.
		{Body: `{"name":"s","kind":"smart","host_filter":"hostname=web"}`,
			WantCode: http.StatusBadRequest},
		// Test 2: A constructed inventory reading another composed one is refused.
		{Body: `{"name":"c","kind":"constructed","input_inventory_ids":["inv_smart"]}`,
			WantCode: http.StatusBadRequest},
		// Test 3: A constructed inventory reading a missing inventory is refused.
		{Body: `{"name":"c","kind":"constructed","input_inventory_ids":["inv_gone"]}`,
			WantCode: http.StatusBadRequest},
		// Test 4: Plugin options outside the constructed plugin are refused.
		{Body: `{"name":"c","kind":"constructed","input_inventory_ids":["inv_web"],` +
			`"source_vars":"plugin: script\n"}`,
			WantCode: http.StatusBadRequest},
		// Test 5: A smart inventory carrying content is refused.
		{Body: `{"name":"s","kind":"smart","host_filter":"name=a","content":"[web]\nx\n"}`,
			WantCode: http.StatusBadRequest},
		// Test 6: A static inventory still needs content.
		{Body: `{"name":"s"}`, WantCode: http.StatusBadRequest},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f := newComposedAccessFixture(t)
			code, body := f.call(t, f.admin, http.MethodPost, "/v1/inventories", test.Body)
			if code != test.WantCode {
				t.Errorf("POST %s = %d %s, want %d", test.Body, code, body, test.WantCode)
			}
		})
	}
}
