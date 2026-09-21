package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/user"
)

// routeRole pins one registered route's minimum role. The map below is the access decision record
// for the whole API surface: every route the mux registers must appear here with the role a
// person decided it takes, so adding a route without deciding who may call it fails this test by
// name instead of silently falling to a default someone discovers in production.
var routeRoles = map[string]user.Role{
	// The full decision record, one row per registered route, generated once from the gate's
	// audited answers and hand-reviewed before freezing. From here every change to a row is a
	// reviewed diff: the gate and this record must agree in both directions, so neither a new
	// route nor a quiet fallthrough change ships without a person deciding it. Routes mounted
	// before the auth gate (health, sign-in, SSO, well-known) still carry the role the gate WOULD
	// answer, purely as a drift alarm; what actually guards them is their mount order.
	"DELETE /v1/credential-types/{id}":        user.Role("admin"),
	"DELETE /v1/credentials/{id}":             user.Role("admin"),
	"DELETE /v1/grants/{id}":                  user.Role("admin"),
	"DELETE /v1/inventories/{id}":             user.Role("admin"),
	"DELETE /v1/inventory-sources/{id}":       user.Role("admin"),
	"DELETE /v1/orgs/{id}":                    user.Role("admin"),
	"DELETE /v1/orgs/{id}/members/{userID}":   user.Role("admin"),
	"DELETE /v1/policies/{id}":                user.Role("admin"),
	"DELETE /v1/projects/{id}":                user.Role("admin"),
	"DELETE /v1/schedules/{id}":               user.Role("admin"),
	"DELETE /v1/teams/{id}":                   user.Role("admin"),
	"DELETE /v1/teams/{id}/members/{userID}":  user.Role("admin"),
	"DELETE /v1/templates/{id}":               user.Role("admin"),
	"DELETE /v1/tokens/{id}":                  user.Role("admin"),
	"DELETE /v1/triggers/{id}":                user.Role("admin"),
	"DELETE /v1/users/{id}":                   user.Role("admin"),
	"GET /.well-known/loomseal.json":          user.Role("viewer"),
	"GET /{$}":                                user.Role("viewer"),
	"GET /auth/oidc/callback":                 user.Role("viewer"),
	"GET /auth/oidc/login":                    user.Role("viewer"),
	"GET /auth/saml/login":                    user.Role("viewer"),
	"GET /auth/saml/metadata":                 user.Role("viewer"),
	"GET /healthz":                            user.Role("viewer"),
	"GET /metrics":                            user.Role("viewer"),
	"GET /readyz":                             user.Role("viewer"),
	"GET /v1/audit":                           user.Role("admin"),
	"GET /v1/audit/bundle":                    user.Role("admin"),
	"GET /v1/audit/register":                  user.Role("admin"),
	"GET /v1/audit/verify":                    user.Role("admin"),
	"GET /v1/auth/me":                         user.Role("viewer"),
	"GET /v1/changes":                         user.Role("viewer"),
	"GET /v1/changes/{change}":                user.Role("viewer"),
	"GET /v1/credential-types":                user.Role("admin"),
	"GET /v1/credential-types/{id}":           user.Role("admin"),
	"GET /v1/credentials":                     user.Role("operator"),
	"GET /v1/doctor":                          user.Role("admin"),
	"GET /v1/drift":                           user.Role("viewer"),
	"GET /v1/estate":                          user.Role("viewer"),
	"GET /v1/estate/diff":                     user.Role("viewer"),
	"GET /v1/fleet":                           user.Role("viewer"),
	"GET /v1/grants":                          user.Role("admin"),
	"GET /v1/hosts/{host}/facts":              user.Role("viewer"),
	"GET /v1/hosts/{host}/runs":               user.Role("viewer"),
	"GET /v1/inventories":                     user.Role("viewer"),
	"GET /v1/inventory-sources":               user.Role("operator"),
	"GET /v1/orgs":                            user.Role("admin"),
	"GET /v1/orgs/{id}/members":               user.Role("admin"),
	"GET /v1/policies":                        user.Role("admin"),
	"GET /v1/projects":                        user.Role("viewer"),
	"GET /v1/projects/{id}/file":              user.Role("viewer"),
	"GET /v1/projects/{id}/files":             user.Role("viewer"),
	"GET /v1/runs":                            user.Role("viewer"),
	"GET /v1/runs/{id}":                       user.Role("viewer"),
	"GET /v1/runs/{id}/compare":               user.Role("viewer"),
	"GET /v1/runs/{id}/events":                user.Role("viewer"),
	"GET /v1/runs/{id}/evidence":              user.Role("operator"),
	"GET /v1/runs/{id}/logs":                  user.Role("viewer"),
	"GET /v1/runs/{id}/receipt":               user.Role("operator"),
	"GET /v1/runs/{id}/shards":                user.Role("viewer"),
	"GET /v1/runs/{id}/steps":                 user.Role("viewer"),
	"GET /v1/runs/{id}/stream":                user.Role("viewer"),
	"GET /v1/schedules":                       user.Role("operator"),
	"GET /v1/schedules/{id}":                  user.Role("operator"),
	"GET /v1/schedules/preview":               user.Role("operator"),
	"GET /v1/tasks":                           user.Role("viewer"),
	"GET /v1/teams":                           user.Role("admin"),
	"GET /v1/teams/{id}/members":              user.Role("admin"),
	"GET /v1/templates":                       user.Role("viewer"),
	"GET /v1/tokens":                          user.Role("admin"),
	"GET /v1/triggers":                        user.Role("operator"),
	"GET /v1/users":                           user.Role("admin"),
	"GET /v1/workers":                         user.Role("viewer"),
	"POST /auth/saml/acs":                     user.Role("admin"),
	"POST /hooks/{token}":                     user.Role("admin"),
	"POST /v1/ai/ask":                         user.Role("viewer"),
	"POST /v1/ai/draft":                       user.Role("operator"),
	"POST /v1/ai/propose-run":                 user.Role("operator"),
	"POST /v1/auth/check":                     user.Role("viewer"),
	"POST /v1/auth/login":                     user.Role("admin"),
	"POST /v1/auth/logout":                    user.Role("viewer"),
	"POST /v1/credential-types":               user.Role("admin"),
	"POST /v1/credentials":                    user.Role("admin"),
	"POST /v1/drift/reconcile":                user.Role("operator"),
	"POST /v1/grants":                         user.Role("admin"),
	"POST /v1/import/{format}":                user.Role("admin"),
	"POST /v1/inventories":                    user.Role("admin"),
	"POST /v1/inventory-sources":              user.Role("admin"),
	"POST /v1/inventory-sources/{id}/refresh": user.Role("admin"),
	"POST /v1/orgs":                           user.Role("admin"),
	"POST /v1/orgs/{id}/members":              user.Role("admin"),
	"POST /v1/pipelines":                      user.Role("operator"),
	"POST /v1/policies":                       user.Role("admin"),
	"POST /v1/projects":                       user.Role("admin"),
	"POST /v1/runs":                           user.Role("operator"),
	"POST /v1/runs/{id}/approve":              user.Role("admin"),
	"POST /v1/runs/{id}/cancel":               user.Role("operator"),
	"POST /v1/runs/{id}/explain":              user.Role("viewer"),
	"POST /v1/runs/{id}/reject":               user.Role("admin"),
	"POST /v1/runs/{id}/relaunch-failed":      user.Role("operator"),
	"POST /v1/runs/{id}/rerun":                user.Role("operator"),
	"POST /v1/runs/{id}/retry":                user.Role("operator"),
	"POST /v1/runs/{id}/stream-ticket":        user.Role("viewer"),
	"POST /v1/schedules":                      user.Role("admin"),
	"POST /v1/teams":                          user.Role("admin"),
	"POST /v1/teams/{id}/members":             user.Role("admin"),
	"POST /v1/templates":                      user.Role("admin"),
	"POST /v1/templates/{id}/launch":          user.Role("operator"),
	"POST /v1/tokens":                         user.Role("admin"),
	"POST /v1/triggers":                       user.Role("admin"),
	"POST /v1/triggers/{id}/rotate-secret":    user.Role("admin"),
	"POST /v1/users":                          user.Role("admin"),
	"PUT /v1/credential-types/{id}":           user.Role("admin"),
	"PUT /v1/credentials/{id}":                user.Role("admin"),
	"PUT /v1/inventories/{id}":                user.Role("admin"),
	"PUT /v1/inventory-sources/{id}":          user.Role("admin"),
	"PUT /v1/policies/{id}":                   user.Role("admin"),
	"PUT /v1/projects/{id}":                   user.Role("admin"),
	"PUT /v1/schedules/{id}":                  user.Role("admin"),
	"PUT /v1/templates/{id}":                  user.Role("admin"),
	"PUT /v1/triggers/{id}":                   user.Role("admin"),
	"PUT /v1/users/{id}":                      user.Role("admin"),
}

// TestEveryRouteHasADecidedRole walks the mux registrations out of server.go and holds each
// against requiredRole and the decision record above, in both directions.
//
// The gate's switch defaults to admin, which fails safe but fails silent: a route meant for
// operators that nobody added to the table simply refuses them, and the first person to notice is
// a customer mid-incident. The reverse drift is worse: a route added to a viewer-read fallthrough
// nobody re-audited. This test derives the route list from the source, so neither direction can
// drift without a named failure, and a new route cannot ship until someone writes its row.
func TestEveryRouteHasADecidedRole(t *testing.T) {
	t.Parallel()
	routes := registeredRoutes(t)
	if len(routes) < 40 {
		t.Fatalf("only %d routes parsed from server.go; the registration shape changed and this "+
			"guard is scanning too little", len(routes))
	}

	idPattern := regexp.MustCompile(`\{[a-z]+(\.\.\.)?\}`)
	seen := map[string]bool{}
	var missing []string
	for _, route := range routes {
		seen[route] = true
		want, decided := routeRoles[route]
		if !decided {
			missing = append(missing, route)
			continue
		}
		method, path, _ := strings.Cut(route, " ")
		concrete := idPattern.ReplaceAllString(path, "x")
		req := httptest.NewRequest(method, concrete, nil)
		if got := requiredRole(req); got != want {
			t.Errorf("%s requires %q, but the decision record says %q: one of them is wrong, and "+
				"the record only moves with a reviewed change to this file", route, got, want)
		}
	}
	for route := range routeRoles {
		if !seen[route] {
			t.Errorf("the decision record lists %s but the mux does not register it: a removed "+
				"route must take its row with it", route)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d route(s) have no row in the decision record; write one per route, with the "+
			"role a person chose:\n  %s", len(missing), strings.Join(missing, "\n  "))
	}
}

// registeredRoutes parses server.go and returns every mux registration pattern.
func registeredRoutes(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", src, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}
	var routes []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
			return true
		}
		if recv, ok := sel.X.(*ast.Ident); !ok || recv.Name != "mux" {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		pattern, err := strconv.Unquote(lit.Value)
		if err != nil || !strings.Contains(pattern, " ") {
			return true
		}
		if method, _, _ := strings.Cut(pattern, " "); method == http.MethodGet ||
			method == http.MethodPost || method == http.MethodPut ||
			method == http.MethodPatch || method == http.MethodDelete ||
			method == http.MethodHead {
			routes = append(routes, pattern)
		}
		return true
	})
	return routes
}
