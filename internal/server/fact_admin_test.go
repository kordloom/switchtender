package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/user"
)

// TestFactCacheReadingCanBeRestrictedToAdmins pins decision twenty. By default an operator granted
// the inventory reads cached facts with secret-looking values masked, and an admin reads them
// whole. With the server setting on, both read paths refuse everyone below admin and name the
// setting, while an admin still reads them.
func TestFactCacheReadingCanBeRestrictedToAdmins(t *testing.T) {
	t.Parallel()
	inventories, facts, store := factsFixture(t)
	operator := Actor{UserID: "user_granted", Role: user.RoleOperator}
	admin := Actor{UserID: "user_admin", Role: user.RoleAdmin}
	tests := []struct {
		Actor      Actor
		WantBody   string
		AdminOnly  bool
		WantStatus int
	}{{ // Test 0: By default an operator with read on the inventory reads them.
		Actor: operator, WantStatus: http.StatusOK, WantBody: "web01",
	}, { // Test 1: With the setting on an operator is refused, and told why.
		Actor: operator, AdminOnly: true, WantStatus: http.StatusForbidden,
		WantBody: "--fact-cache-admin-only",
	}, { // Test 2: With the setting on an admin still reads them.
		Actor: admin, AdminOnly: true, WantStatus: http.StatusOK, WantBody: "web01",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			authz := restrictedAuthz(t, "user_granted", "inv_1")
			for _, read := range []struct {
				path    string
				handler http.Handler
			}{{
				path: "/v1/inventories/inv_1/facts",
				handler: listFactsHandler(inventories, facts, store, authz, test.AdminOnly,
					zap.NewNop()),
			}, {
				path: "/v1/inventories/inv_1/facts/web01",
				handler: hostCachedFactsHandler(inventories, facts, store, authz, test.AdminOnly,
					zap.NewNop()),
			}} {
				req := withActor(t, http.MethodGet, read.path, test.Actor)
				req.SetPathValue("id", "inv_1")
				req.SetPathValue("host", "web01")
				rec := httptest.NewRecorder()
				read.handler.ServeHTTP(rec, req)
				if rec.Code != test.WantStatus || !strings.Contains(rec.Body.String(), test.WantBody) {
					t.Errorf("%s: status = %d body %s, want %d containing %q", read.path, rec.Code,
						rec.Body.String(), test.WantStatus, test.WantBody)
				}
			}
		})
	}
}
