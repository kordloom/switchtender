package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/user"
)

// TestConstructedDefinitionsAreAdminOnly pins who may write a constructed inventory or have an
// unsaved one evaluated: an admin in person. An operator, an agent of any role, and an operator
// holding a manage grant are refused before anything is listed. A saved constructed inventory
// still previews for an operator, and its delete stays delegable.
func TestConstructedDefinitionsAreAdminOnly(t *testing.T) {
	t.Parallel()
	const unsaved = `{"kind":"constructed","input_inventory_ids":["inv_web"]}`
	tests := []struct {
		Caller     string
		Method     string
		Path       string
		Body       string
		WantCode   int
		WantListed bool
		WantInBody string
	}{{ // Test 0: An operator's unsaved constructed preview is refused at the role.
		Caller: "operator", Method: http.MethodPost, Path: "/v1/inventories/preview", Body: unsaved,
		WantCode: http.StatusForbidden,
	}, { // Test 1: So is an agent's acting for the operator.
		Caller: "operator agent", Method: http.MethodPost, Path: "/v1/inventories/preview",
		Body: unsaved, WantCode: http.StatusForbidden,
	}, { // Test 2: So is an agent bound to an admin, whose token the agent cap holds below admin.
		Caller: "admin agent", Method: http.MethodPost, Path: "/v1/inventories/preview",
		Body: unsaved, WantCode: http.StatusForbidden,
	}, { // Test 3: An admin's unsaved preview over clean inputs is evaluated.
		Caller: "admin", Method: http.MethodPost, Path: "/v1/inventories/preview", Body: unsaved,
		WantCode: http.StatusOK, WantListed: true,
	}, { // Test 4: The unsaved smart preview is admin only too, since the route is.
		Caller: "operator", Method: http.MethodPost, Path: "/v1/inventories/preview",
		Body: `{"kind":"smart","host_filter":"groups__name=web"}`, WantCode: http.StatusForbidden,
	}, { // Test 5: A manage grant does not cover an update of a constructed definition.
		Caller: "operator", Method: http.MethodPut, Path: "/v1/inventories/inv_built",
		Body:     `{"name":"built","kind":"constructed","input_inventory_ids":["inv_web"]}`,
		WantCode: http.StatusForbidden, WantInBody: "only an admin",
	}, { // Test 6: Nor a rename.
		Caller: "operator", Method: http.MethodPut, Path: "/v1/inventories/inv_built",
		Body:     `{"name":"renamed","kind":"constructed","input_inventory_ids":["inv_web","inv_db"]}`,
		WantCode: http.StatusForbidden, WantInBody: "only an admin",
	}, { // Test 7: Nor turning it into another kind.
		Caller: "operator", Method: http.MethodPut, Path: "/v1/inventories/inv_built",
		Body:     `{"name":"built","kind":"smart","host_filter":"groups__name=web"}`,
		WantCode: http.StatusForbidden, WantInBody: "only an admin",
	}, { // Test 8: An admin updates it.
		Caller: "admin", Method: http.MethodPut, Path: "/v1/inventories/inv_built",
		Body:     `{"name":"built","kind":"constructed","input_inventory_ids":["inv_web"]}`,
		WantCode: http.StatusOK,
	}, { // Test 9: An admin's agent cannot create one.
		Caller: "admin agent", Method: http.MethodPost, Path: "/v1/inventories",
		Body:     `{"name":"new","kind":"constructed","input_inventory_ids":["inv_web"]}`,
		WantCode: http.StatusForbidden,
	}, { // Test 10: An admin creates one.
		Caller: "admin", Method: http.MethodPost, Path: "/v1/inventories",
		Body:     `{"name":"new","kind":"constructed","input_inventory_ids":["inv_web"]}`,
		WantCode: http.StatusCreated,
	}, { // Test 11: A saved constructed inventory still previews for an operator.
		Caller: "operator", Method: http.MethodPost, Path: "/v1/inventories/inv_built/preview",
		WantCode: http.StatusOK, WantListed: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f := newComposedAccessFixture(t)
			bearer := map[string]string{
				"operator": f.operator, "admin": f.admin,
				"operator agent": f.agents[user.RoleOperator], "admin agent": f.agents[user.RoleAdmin],
			}[test.Caller]
			code, body := f.call(t, bearer, test.Method, test.Path, test.Body)
			if code != test.WantCode {
				t.Fatalf("%s %s as %s = %d %s, want %d", test.Method, test.Path, test.Caller, code,
					body, test.WantCode)
			}
			if !strings.Contains(string(body), test.WantInBody) {
				t.Errorf("body = %s, want it to hold %q", body, test.WantInBody)
			}
			if listed := f.runner.Lists() > 0; listed != test.WantListed {
				t.Errorf("listings run = %d, want any: %t", f.runner.Lists(), test.WantListed)
			}
		})
	}
}

// TestInventorySaveRefusesAHiddenCharacterInTheName pins that creating or renaming an inventory
// refuses a name holding a direction override or a zero-width character, so the name an approver
// reads in a held run is the name that was saved.
func TestInventorySaveRefusesAHiddenCharacterInTheName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Method is POST for a create and PUT for an update.
		Method string
		// Body is the request body.
		Body string
		// WantCode is the status the save answers with.
		WantCode int
		// WantInBody is text the answer must hold.
		WantInBody string
	}{{ // Test 0: A create whose name holds a direction override.
		Method: http.MethodPost, Body: `{"name":"prod\u202ebew","content":"web1"}`,
		WantCode: http.StatusBadRequest, WantInBody: "U+202E RIGHT-TO-LEFT OVERRIDE at character 5",
	}, { // Test 1: A rename to a name holding a zero-width space.
		Method: http.MethodPut, Body: `{"name":"pro\u200bd","content":"web1"}`,
		WantCode: http.StatusBadRequest, WantInBody: "U+200B ZERO WIDTH SPACE at character 4",
	}, { // Test 2: A plain name is saved.
		Method: http.MethodPost, Body: `{"name":"prod","content":"web1"}`,
		WantCode: http.StatusCreated,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := inventory.NewMemStore()
			if err := store.Save(context.Background(), &inventory.Inventory{
				ID: "inv_1", Name: "prod", Content: "web1",
			}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			h := createInventoryHandler(store, &authorizer{}, nil, zap.NewNop())
			path := "/v1/inventories"
			if test.Method == http.MethodPut {
				h = updateInventoryHandler(store, &authorizer{}, nil, zap.NewNop())
				path += "/inv_1"
			}
			req := httptest.NewRequest(test.Method, path, strings.NewReader(test.Body))
			req.SetPathValue("id", "inv_1")
			req = req.WithContext(context.WithValue(req.Context(), actorKey{},
				Actor{UserID: "u", Role: user.RoleAdmin}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != test.WantCode || !strings.Contains(rec.Body.String(), test.WantInBody) {
				t.Errorf("%s %s = %d %s, want %d holding %q", test.Method, path, rec.Code,
					rec.Body.String(), test.WantCode, test.WantInBody)
			}
		})
	}
}
