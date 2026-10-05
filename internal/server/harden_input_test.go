package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/team"
	"github.com/kordloom/switchtender/internal/user"
)

// hardenRequest is one request a hardening table sends and what it must be answered.
type hardenRequest struct {
	// Name labels the case.
	Name string
	// Method is the HTTP method.
	Method string
	// Target is the request target, path and query, percent escapes included.
	Target string
	// Body is the JSON body, empty for none.
	Body string
	// Header holds extra request headers.
	Header map[string]string
	// WantStatus is the status the request must be answered with.
	WantStatus int
	// WantBody is text the answer must contain, empty to check the status only.
	WantBody string
}

// runHardenTable sends every request in tests to a server on each backend and checks its answer.
// Each backend is seeded first with a team, an organization, and an account the requests refer to.
func runHardenTable(t *testing.T, tests []hardenRequest) {
	t.Helper()
	for _, backend := range hardenBackends(false) {
		t.Run(backend.Name, func(t *testing.T) {
			t.Parallel()
			db, _ := backend.Open(t)
			hardenSeed(t, db)
			h, token := hardenServer(t, db)
			for testNum, test := range tests {
				rec := hardenCall(h, token, test.Method, test.Target, test.Body, test.Header)
				if diff := cmp.Diff(test.WantStatus, rec.Code); diff != "" {
					t.Errorf("test %d %s: status mismatch (-want +got):\n%s\nbody: %s", testNum,
						test.Name, diff, strings.TrimSpace(rec.Body.String()))
					continue
				}
				if test.WantBody != "" && !strings.Contains(rec.Body.String(), test.WantBody) {
					t.Errorf("test %d %s: body %q does not say %q", testNum, test.Name,
						strings.TrimSpace(rec.Body.String()), test.WantBody)
				}
			}
		})
	}
}

// hardenSeed stores the team, organization, and account the hardening tables refer to.
func hardenSeed(t *testing.T, db hardenDB) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	if err := db.teams.Save(ctx, &team.Team{ID: "team_harden", Name: "harden",
		CreatedAt: now}); err != nil {
		t.Fatalf("save team: %v", err)
	}
	if err := db.orgs.Save(ctx, &org.Org{ID: "org_harden", Name: "harden",
		CreatedAt: now}); err != nil {
		t.Fatalf("save org: %v", err)
	}
	u, err := user.New("harden-operator", "a-long-enough-password", user.RoleOperator)
	if err != nil {
		t.Fatalf("new user: %v", err)
	}
	if err := db.users.Save(ctx, u); err != nil {
		t.Fatalf("save user: %v", err)
	}
}

// TestUnstorableTextIsRefusedOnEveryBackend sends a NUL byte and bytes that are not UTF-8 through
// a request's path, its query, and its JSON body, on SQLite and on PostgreSQL.
//
// SQLite stores both and PostgreSQL refuses both with SQLSTATE 22021, so the same request was a 404
// or a 201 on one backend and a 500 on the other: a NUL in a run id's path segment, in a list
// filter, or in a team's name all reached the store. Each is now a 400 before anything stores it,
// naming the part of the request that holds it, on both.
func TestUnstorableTextIsRefusedOnEveryBackend(t *testing.T) {
	t.Parallel()
	runHardenTable(t, []hardenRequest{{ // Test 0: A NUL in a path segment.
		Name: "path NUL", Method: http.MethodGet, Target: "/v1/runs/%00",
		WantStatus: http.StatusBadRequest, WantBody: "the request path holds a NUL byte",
	}, { // Test 1: A path segment that is not UTF-8.
		Name: "path not UTF-8", Method: http.MethodGet, Target: "/v1/runs/%FF",
		WantStatus: http.StatusBadRequest, WantBody: "not valid UTF-8",
	}, { // Test 2: A NUL in the middle of a longer path.
		Name: "host path NUL", Method: http.MethodGet, Target: "/v1/hosts/web%00one/runs",
		WantStatus: http.StatusBadRequest, WantBody: "the request path",
	}, { // Test 3: A NUL in a query value, named by its parameter.
		Name: "query NUL", Method: http.MethodGet, Target: "/v1/runs?status=%00",
		WantStatus: http.StatusBadRequest, WantBody: `the query parameter \"status\"`,
	}, { // Test 4: A query parameter name that is not UTF-8.
		Name: "query name", Method: http.MethodGet, Target: "/v1/runs?%FF=1",
		WantStatus: http.StatusBadRequest, WantBody: "a query parameter name",
	}, { // Test 5: An ordinary missing run is still a 404, so the guard refuses only bad text.
		Name: "ordinary path", Method: http.MethodGet, Target: "/v1/runs/run_missing",
		WantStatus: http.StatusNotFound,
	}, { // Test 6: Text that is valid UTF-8 beyond ASCII passes the guard.
		Name: "multibyte query", Method: http.MethodGet, Target: "/v1/runs?actor=%C3%A9",
		WantStatus: http.StatusOK,
	}, { // Test 7: An escaped NUL in an inventory's name, named in the refusal.
		Name: "inventory name", Method: http.MethodPost, Target: "/v1/inventories",
		Body:       `{"name":"fleet\u0000east","content":"web1\n"}`,
		WantStatus: http.StatusBadRequest, WantBody: `the field \"name\" in the request body holds a NUL`,
	}, { // Test 8: An escaped NUL in a team's name.
		Name: "team name", Method: http.MethodPost, Target: "/v1/teams",
		Body: `{"name":"ops\u0000"}`, WantStatus: http.StatusBadRequest, WantBody: `\"name\"`,
	}, { // Test 9: An escaped NUL in a username.
		Name: "username", Method: http.MethodPost, Target: "/v1/users",
		Body:       `{"username":"ops\u0000","password":"a-long-enough-password","role":"viewer"}`,
		WantStatus: http.StatusBadRequest, WantBody: `\"username\"`,
	}, { // Test 10: An escaped backslash before u0000 is text, not a NUL, and is stored.
		Name: "escaped backslash", Method: http.MethodPost, Target: "/v1/teams",
		Body: `{"name":"ops\\u0000"}`, WantStatus: http.StatusCreated,
	}, { // Test 11: The same body without the NUL is created.
		Name: "clean inventory", Method: http.MethodPost, Target: "/v1/inventories",
		Body: `{"name":"fleet-east","content":"web1\n"}`, WantStatus: http.StatusCreated,
	}})
}

// TestLongValuesBoundForIndexedColumnsOnEveryBackend sends values longer than their bound into the
// fields stored under an index, on SQLite and on PostgreSQL.
//
// PostgreSQL refuses an index entry past about 2.7 kilobytes. Text that compresses fits, which is
// why a key of one repeated letter passed, but four thousand random characters did not: the submit,
// the account, the grant, and the membership were each answered 500, and a token minted with such a
// name had every change it asked for refused, because the audit trail could not index its actor.
// Each field now has a bound far under the limit and is refused past it with a 400 stating the
// bound, and a value at the bound is stored on both backends.
func TestLongValuesBoundForIndexedColumnsOnEveryBackend(t *testing.T) {
	t.Parallel()
	long := randomText(1, 4000)
	runHardenTable(t, []hardenRequest{{ // Test 0: An Idempotency-Key past the bound.
		Name: "long key", Method: http.MethodPost, Target: "/v1/runs", Body: `{"playbook":"site.yml"}`,
		Header:     map[string]string{"Idempotency-Key": long},
		WantStatus: http.StatusBadRequest, WantBody: "at most 255 bytes",
	}, { // Test 1: An Idempotency-Key one byte past the bound.
		Name: "key at bound plus one", Method: http.MethodPost, Target: "/v1/runs",
		Body:       `{"playbook":"site.yml"}`,
		Header:     map[string]string{"Idempotency-Key": randomText(2, 256)},
		WantStatus: http.StatusBadRequest, WantBody: "at most 255 bytes",
	}, { // Test 2: An Idempotency-Key at the bound is accepted.
		Name: "key at bound", Method: http.MethodPost, Target: "/v1/runs",
		Body:       `{"playbook":"site.yml"}`,
		Header:     map[string]string{"Idempotency-Key": randomText(3, 255)},
		WantStatus: http.StatusAccepted,
	}, { // Test 3: A queue name past the bound.
		Name: "long queue", Method: http.MethodPost, Target: "/v1/runs",
		Body:       fmt.Sprintf(`{"playbook":"site.yml","queue":%q}`, long),
		WantStatus: http.StatusBadRequest, WantBody: "a queue name may be at most 255 bytes",
	}, { // Test 4: A template's queue past the bound.
		Name: "long template queue", Method: http.MethodPost, Target: "/v1/templates",
		Body:       fmt.Sprintf(`{"name":"t","playbook":"site.yml","queue":%q}`, long),
		WantStatus: http.StatusBadRequest, WantBody: "a queue name may be at most 255 bytes",
	}, { // Test 5: A username past the bound.
		Name: "long username", Method: http.MethodPost, Target: "/v1/users",
		Body: fmt.Sprintf(`{"username":%q,"password":"a-long-enough-password","role":"viewer"}`,
			long),
		WantStatus: http.StatusBadRequest, WantBody: "a username may be at most 255 bytes",
	}, { // Test 6: A username at the bound is created.
		Name: "username at bound", Method: http.MethodPost, Target: "/v1/users",
		Body: fmt.Sprintf(`{"username":%q,"password":"a-long-enough-password","role":"viewer"}`,
			randomText(4, 255)),
		WantStatus: http.StatusCreated,
	}, { // Test 7: A rename past the bound.
		Name: "long rename", Method: http.MethodPut, Target: "/v1/users/user_missing",
		Body:       fmt.Sprintf(`{"username":%q,"role":"viewer"}`, long),
		WantStatus: http.StatusBadRequest, WantBody: "a username may be at most 255 bytes",
	}, { // Test 8: A token name past the bound.
		Name: "long token name", Method: http.MethodPost, Target: "/v1/tokens",
		Body:       fmt.Sprintf(`{"name":%q,"username":"harden-operator"}`, long),
		WantStatus: http.StatusBadRequest, WantBody: "a token name may be at most 255 bytes",
	}, { // Test 9: A grant subject past the bound.
		Name: "long grant subject", Method: http.MethodPost, Target: "/v1/grants",
		Body:       fmt.Sprintf(`{"subject":"team_%s","object":"tpl_x","access":"use"}`, long),
		WantStatus: http.StatusBadRequest, WantBody: "subject may be at most 512 bytes",
	}, { // Test 10: A grant object past the bound.
		Name: "long grant object", Method: http.MethodPost, Target: "/v1/grants",
		Body:       fmt.Sprintf(`{"subject":"team_harden","object":"tpl_%s","access":"use"}`, long),
		WantStatus: http.StatusBadRequest, WantBody: "object may be at most 512 bytes",
	}, { // Test 11: A team member id past the bound.
		Name: "long team member", Method: http.MethodPost, Target: "/v1/teams/team_harden/members",
		Body:       fmt.Sprintf(`{"user_id":"user_%s"}`, long),
		WantStatus: http.StatusBadRequest, WantBody: "user_id may be at most 512 bytes",
	}, { // Test 12: An organization member id past the bound.
		Name: "long org member", Method: http.MethodPost, Target: "/v1/orgs/org_harden/members",
		Body:       fmt.Sprintf(`{"user_id":"user_%s"}`, long),
		WantStatus: http.StatusBadRequest, WantBody: "user_id may be at most 512 bytes",
	}})
}

// TestAnImportHoldingUnstorableTextWritesNothingOnEveryBackend applies an AWX export whose project
// name holds an escaped NUL, on SQLite and on PostgreSQL.
//
// An export is a file from somebody else's system. On PostgreSQL its apply failed with a 500
// partway through, after the objects ahead of the bad one were already written, and on SQLite the
// byte was stored. The plan is now checked before anything is written, the refusal names the object
// and the field, and nothing from the export is left behind.
func TestAnImportHoldingUnstorableTextWritesNothingOnEveryBackend(t *testing.T) {
	t.Parallel()
	export := `{"projects":[` +
		`{"name":"Ops","scm_type":"git","scm_url":"https://git.example.com/ops.git"},` +
		`{"name":"We\u0000b","scm_type":"git","scm_url":"https://git.example.com/web.git"}],` +
		`"inventory":[{"name":"Production","hosts":[{"name":"web1"}]}]}`
	runHardenTable(t, []hardenRequest{{ // Test 0: The apply is refused, naming the project and field.
		Name: "apply", Method: http.MethodPost, Target: "/v1/import/awx?apply=true", Body: export,
		WantStatus: http.StatusBadRequest,
		WantBody:   `the project \"We�b\" holds a NUL byte or text that is not valid UTF-8 in Name`,
	}, { // Test 1: No project of the export was written.
		Name: "projects", Method: http.MethodGet, Target: "/v1/projects", WantStatus: http.StatusOK,
		WantBody: `"count":0`,
	}, { // Test 2: Nor was its inventory.
		Name: "inventories", Method: http.MethodGet, Target: "/v1/inventories",
		WantStatus: http.StatusOK, WantBody: `"count":0`,
	}})
}
