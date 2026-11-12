package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/mcp"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/user"
)

const (
	// splitRawPath is the inventory path a caller writes in these tests. Nothing exists at it: the
	// lister double answers for every path, so a listing of it is seen with no file to read.
	splitRawPath = "/srv/inventories/prod.ini"
	// splitSeededHook is the token of the webhook seeded on the managed template.
	splitSeededHook = "managed-hook"
	// splitRawRun is a run body that splits the path.
	splitRawRun = `{"playbook":"site.yml","inventory":"` + splitRawPath + `","shards":2}`
	// splitStoredRun is a run body that splits the stored inventory.
	splitStoredRun = `{"playbook":"site.yml","inventory_id":"inv_fleet","shards":2}`
	// splitRawTemplate is a template body whose saved run splits the path.
	splitRawTemplate = `{"name":"deploy","playbook":"site.yml","inventory":"` + splitRawPath +
		`","shards":2}`
	// splitStoredTemplate is a template body whose saved run splits the stored inventory.
	splitStoredTemplate = `{"name":"deploy","playbook":"site.yml","inventory_id":"inv_fleet",` +
		`"shards":2}`
	// splitRawSchedule is a schedule body that splits the path on a timetable.
	splitRawSchedule = `{"name":"fleet nightly","cron":"0 2 * * *","playbook":"site.yml",` +
		`"inventory":"` + splitRawPath + `","shards":2}`
	// splitTemplateSchedule is a schedule body that fires the template that splits the path.
	splitTemplateSchedule = `{"name":"fleet nightly","cron":"0 2 * * *","template_id":"tpl_split"}`
	// splitRawTrigger is a trigger body that fires the template that splits the path.
	splitRawTrigger = `{"name":"fleet push","template_id":"tpl_split"}`
	// splitRawWorkflow is a workflow body with one step that names the path.
	splitRawWorkflow = `{"name":"release","steps":[{"name":"deploy","playbook":"site.yml",` +
		`"inventory":"` + splitRawPath + `"}]}`
	// splitSteppedTemplate is a template body that asks for shards on a workflow.
	splitSteppedTemplate = `{"name":"deploy","shards":2,"steps":[{"name":"deploy",` +
		`"playbook":"site.yml","inventory":"` + splitRawPath + `"}]}`
)

// splitPathLister is a Runner that lists the same hosts for every inventory and records each
// inventory path and limit it was handed, by either lister function, so a test can tell whether a
// listing was reached and what reached it.
type splitPathLister struct {
	// mu guards paths and limits.
	mu sync.Mutex
	// paths records the inventory argument of each listing.
	paths []string
	// limits records the limit argument of each listing.
	limits []string
}

// Run reports success without doing anything.
func (l *splitPathLister) Run(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
	return roundhouse.Result{ExitCode: 0}, nil
}

// Hosts returns four hosts, recording the inventory path and the limit it was given.
func (l *splitPathLister) Hosts(_ context.Context, path, limit string) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paths, l.limits = append(l.paths, path), append(l.limits, limit)
	return []string{"a", "b", "c", "d"}, nil
}

// ListInventory records the sources and the limit it was given and returns an empty listing.
func (l *splitPathLister) ListInventory(_ context.Context, sources []string, limit string) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paths, l.limits = append(l.paths, sources...), append(l.limits, limit)
	return []byte(`{"_meta":{"hostvars":{}}}`), nil
}

// listed returns how many listings were asked for and whether path was one of them.
func (l *splitPathLister) listed(path string) (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.limits), slices.Contains(l.paths, path)
}

// fileLimits returns the limits handed to a listing that hold an at sign.
func (l *splitPathLister) fileLimits() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var found []string
	for _, limit := range l.limits {
		if strings.Contains(limit, "@") {
			found = append(found, limit)
		}
	}
	return found
}

// pathSplitServer is a server whose dispatcher lists hosts through a double that records what it
// was handed, with an operator, an agent acting for that operator, and an admin.
type pathSplitServer struct {
	*agentHoldServer
	// Lister records the inventory paths and limits the dispatcher listed.
	Lister *splitPathLister
	// Dispatcher is what the server submits to and what a schedule fires through.
	Dispatcher *dispatch.Dispatcher
}

// newPathSplitServer serves a fresh in-memory install seeded by pathSplitSeed. The operator holds
// a manage grant on the template tpl_managed, which is what lets a caller below admin save into a
// template at all.
func newPathSplitServer(t *testing.T) *pathSplitServer {
	t.Helper()
	ctx := context.Background()
	db := agentHoldBackends()[0].Open(t)
	lister := &splitPathLister{}
	d := dispatch.New(db.runs, lister, zap.NewNop(), dispatch.WithNoJanitor(),
		dispatch.WithInventories(db.inventories), dispatch.WithPolicies(db.policies))
	t.Cleanup(d.Close)

	account := func(name string, role user.Role) *user.User {
		u, err := user.New(name, "a long enough password", role)
		if err != nil {
			t.Fatalf("user.New(%s) error = %v", name, err)
		}
		if err := db.users.Save(ctx, u); err != nil {
			t.Fatalf("users.Save(%s) error = %v", name, err)
		}
		return u
	}
	lead, approver := account("dev-lead", user.RoleOperator), account("approver", user.RoleAdmin)
	mint := func(name, userID, kind string) string {
		plain, tok, err := auth.New(name)
		if err != nil {
			t.Fatalf("auth.New(%s) error = %v", name, err)
		}
		tok.UserID, tok.Kind = userID, kind
		if err := db.tokens.Save(ctx, tok); err != nil {
			t.Fatalf("tokens.Save(%s) error = %v", name, err)
		}
		return plain
	}
	grants := grant.NewMemStore()
	if err := grants.Save(ctx, &grant.Grant{ID: "grant_tpl_managed", Subject: lead.ID,
		Object: "tpl_managed", Access: grant.AccessManage, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("grants.Save() error = %v", err)
	}
	s := &pathSplitServer{Lister: lister, Dispatcher: d, agentHoldServer: &agentHoldServer{
		DB:     db,
		Agent:  mint("release-agent", lead.ID, auth.KindAgent),
		Person: mint("dev-lead-cli", lead.ID, ""),
		Admin:  mint("approver-cli", approver.ID, ""),
	}}
	s.Handler = New(db.runs, d, zap.NewNop(), WithTokens(db.tokens), WithUsers(db.users),
		WithAudit(audit.NewMemStore()), WithApprover(d), WithRetrier(d), WithGrants(grants, false),
		WithTemplates(db.templates), WithSchedules(db.schedules),
		WithTriggers(db.triggers, sharedSealer("agent-hold-pass", "agent-hold-salt")),
		WithInventories(db.inventories), WithProjects(db.projects),
		WithPolicies(db.policies)).Handler()
	ts := httptest.NewServer(s.Handler)
	t.Cleanup(ts.Close)
	s.URL = ts.URL
	pathSplitSeed(t, db)
	return s
}

// pathSplitSeed stores what the entries start from: a stored inventory, a plain template the
// operator manages, a template that splits the path, one that splits the stored inventory, a saved
// workflow whose step names the path, a finished split of the path, one finished run whose limit
// names a file, a webhook on the managed template, and a schedule that fires a plain run.
func pathSplitSeed(t *testing.T, db agentHoldDB) {
	t.Helper()
	ctx := context.Background()
	if err := db.inventories.Save(ctx, &inventory.Inventory{ID: "inv_fleet", Name: "fleet",
		Content: "a\nb\nc\nd\n"}); err != nil {
		t.Fatalf("inventories.Save() error = %v", err)
	}
	for _, tpl := range []*template.Template{
		{ID: "tpl_managed", Name: "deploy", Playbook: "site.yml", Inventory: "hosts.ini"},
		{ID: "tpl_split", Name: "fleet deploy", Playbook: "site.yml", Inventory: splitRawPath,
			Shards: 2},
		{ID: "tpl_stored", Name: "stored fleet deploy", Playbook: "site.yml",
			InventoryID: "inv_fleet", Shards: 2},
		{ID: "tpl_flow", Name: "release flow", Steps: []run.PipelineStep{
			{Name: "deploy", Playbook: "site.yml", Inventory: splitRawPath}}},
	} {
		if err := db.templates.Save(ctx, tpl); err != nil {
			t.Fatalf("templates.Save(%s) error = %v", tpl.ID, err)
		}
	}
	at, count := time.Now().Add(-time.Hour), 2
	for _, done := range []*run.Run{
		{ID: "run_split_raw", Playbook: "site.yml", Inventory: splitRawPath, Kind: run.KindSplit,
			ShardCount: &count},
		{ID: "run_limit_file", Playbook: "site.yml", Inventory: "hosts.ini",
			Limit: "web01,@/etc/shells"},
	} {
		done.Status, done.CreatedAt = run.StatusSucceeded, at
		done.Actor, done.ActorType = "approver-cli", "token"
		if err := db.runs.Save(ctx, done); err != nil {
			t.Fatalf("runs.Save(%s) error = %v", done.ID, err)
		}
	}
	if err := db.triggers.Save(ctx, &trigger.Trigger{ID: "trg_managed", Name: "on push",
		TemplateID: "tpl_managed", TokenHash: trigger.HashToken(splitSeededHook),
		CreatedAt: time.Now()}); err != nil {
		t.Fatalf("triggers.Save() error = %v", err)
	}
	if err := db.schedules.Save(ctx, &schedule.Schedule{ID: "sch_plain", Name: "nightly",
		Cron: "0 2 * * *", Playbook: "site.yml", Inventory: "hosts.ini", Enabled: true,
		CreatedAt: time.Now()}); err != nil {
		t.Fatalf("schedules.Save() error = %v", err)
	}
}

// send makes one request as the bearer of token and returns the status and the body. With tool set
// it calls that MCP tool with body as its arguments instead. A tool reports the API's status only
// when it is refused, at the head of its error, so a call that worked reads as the 202 the tools'
// endpoints answer.
func (s *pathSplitServer) send(t *testing.T, token, tool, method, path, body string) (int, string) {
	t.Helper()
	if tool == "" {
		return s.call(token, method, path, body)
	}
	client, err := mcp.NewClient(s.URL, token, 5*time.Second)
	if err != nil {
		t.Fatalf("mcp.NewClient() error = %v", err)
	}
	for _, candidate := range mcp.Tools(client, mcp.Options{AllowAdhoc: true}) {
		if candidate.Name != tool {
			continue
		}
		reply, err := candidate.Run(context.Background(), json.RawMessage(body))
		if err == nil {
			return http.StatusAccepted, reply
		}
		code := 0
		_, _ = fmt.Sscanf(err.Error(), "%d", &code)
		return code, err.Error()
	}
	t.Fatalf("the MCP server does not offer %s", tool)
	return 0, ""
}

// fireLaunchers fires everything on the install that launches a run with nobody present: the
// webhook seeded on the managed template, a webhook reply just created, and every stored schedule.
// A request can save into any of them, so what a request let through is what these then launch.
func (s *pathSplitServer) fireLaunchers(t *testing.T, reply string) {
	t.Helper()
	ctx := context.Background()
	var created struct {
		// WebhookPath is where a trigger the request created is delivered to.
		WebhookPath string `json:"webhook_path"`
	}
	hooks := []string{"/hooks/" + splitSeededHook}
	if json.Unmarshal([]byte(reply), &created) == nil && created.WebhookPath != "" {
		hooks = append(hooks, created.WebhookPath)
	}
	for hookNum, hook := range hooks {
		rec := httptest.NewRecorder()
		s.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, hook, nil))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("delivery to webhook %d = %d, want 202: %s", hookNum, rec.Code,
				rec.Body.String())
		}
	}

	stored, err := s.DB.schedules.List(ctx)
	if err != nil {
		t.Fatalf("schedules.List() error = %v", err)
	}
	due := time.Now().Add(-time.Minute)
	for _, sc := range stored {
		sc.NextRunAt = &due
		if err := s.DB.schedules.Save(ctx, sc); err != nil {
			t.Fatalf("schedules.Save(%s) error = %v", sc.ID, err)
		}
	}
	scheduler := schedule.NewScheduler(s.DB.schedules, s.Dispatcher, zap.NewNop(),
		schedule.WithTemplates(s.DB.templates), schedule.WithInterval(5*time.Millisecond))
	scheduler.Start()
	defer scheduler.Close()
	unfired := func(sc *schedule.Schedule) bool { return sc.LastRunAt == nil }
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if stored, err = s.DB.schedules.List(ctx); err != nil {
			t.Fatalf("schedules.List() error = %v", err)
		}
		if !slices.ContainsFunc(stored, unfired) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d schedules were due and not every one fired", len(stored))
		}
	}
	for _, sc := range stored {
		if sc.LastRunID == "" {
			t.Errorf("schedule %q fired no run: %s", sc.Name, sc.LastError)
		}
	}
}

// token returns the credential of the named caller: operator, agent, or admin.
func (s *pathSplitServer) token(t *testing.T, caller string) string {
	t.Helper()
	switch caller {
	case "operator":
		return s.Person
	case "agent":
		return s.Agent
	case "admin":
		return s.Admin
	}
	t.Fatalf("no caller named %q", caller)
	return ""
}

// TestPathSplitNeedsAnAdminAtEveryEntry pins who may have a path of their choosing listed.
//
// A split lists its inventory's hosts before the run exists, so before any policy, hold, or
// approval, and the listing opens the inventory as the user the server runs as. With a stored
// inventory that is content somebody with the right to save it put there. With a path it is
// whatever the sender typed, and the answer said whether a file exists on the server. So a split of
// a path is taken from an admin only, at every place a caller hands one in or saves one: a submit,
// a rerun, a template save and its launch, a schedule, a trigger, and the MCP tools, which launch
// through the same endpoints. A schedule and a webhook fire with nobody to ask, so each row then
// fires every one of them on the install, and what counts is whether the path was ever listed. A
// workflow step cannot split, so no row of it lists for anybody. The MCP server refuses an admin's
// token, so it has no admin row.
//
//nolint:funlen // Test function.
func TestPathSplitNeedsAnAdminAtEveryEntry(t *testing.T) {
	t.Parallel()
	const post, put = http.MethodPost, http.MethodPut
	tests := []struct {
		// Entry names the place the run is handed in or saved.
		Entry string
		// Caller is who asks: operator, agent, or admin.
		Caller string
		// Tool, when set, is the MCP tool called with Body as its arguments.
		Tool string
		// Method is the request method.
		Method string
		// Path is the request path.
		Path string
		// Body is the request body.
		Body string
		// WantCode is the status the request answers.
		WantCode int
		// WantListings is how many host listings the request and the launchers after it ask for.
		WantListings int
		// WantRawListed is whether the path the caller wrote was listed.
		WantRawListed bool
	}{{ // Test 0: An operator's split of a path is refused.
		Entry: "submit", Caller: "operator", Method: post, Path: "/v1/runs", Body: splitRawRun,
		WantCode: http.StatusForbidden,
	}, { // Test 1: An agent's is refused.
		Entry: "submit", Caller: "agent", Method: post, Path: "/v1/runs", Body: splitRawRun,
		WantCode: http.StatusForbidden,
	}, { // Test 2: An admin's lists the path.
		Entry: "submit", Caller: "admin", Method: post, Path: "/v1/runs", Body: splitRawRun,
		WantCode: http.StatusAccepted, WantListings: 1, WantRawListed: true,
	}, { // Test 3: An operator's split of a stored inventory lists that inventory.
		Entry: "submit", Caller: "operator", Method: post, Path: "/v1/runs", Body: splitStoredRun,
		WantCode: http.StatusAccepted, WantListings: 1,
	}, { // Test 4: An agent's split of a stored inventory lists it and waits for a person.
		Entry: "submit", Caller: "agent", Method: post, Path: "/v1/runs", Body: splitStoredRun,
		WantCode: http.StatusAccepted, WantListings: 1,
	}, { // Test 5: An operator's single run names the path and lists nothing.
		Entry: "submit", Caller: "operator", Method: post, Path: "/v1/runs",
		Body:     `{"playbook":"site.yml","inventory":"` + splitRawPath + `"}`,
		WantCode: http.StatusAccepted,
	}, { // Test 6: An operator's rerun of a split of a path is refused.
		Entry: "rerun", Caller: "operator", Method: post, Path: "/v1/runs/run_split_raw/rerun",
		Body: `{}`, WantCode: http.StatusForbidden,
	}, { // Test 7: An agent's is refused.
		Entry: "rerun", Caller: "agent", Method: post, Path: "/v1/runs/run_split_raw/rerun",
		Body: `{}`, WantCode: http.StatusForbidden,
	}, { // Test 8: An admin's lists the path again.
		Entry: "rerun", Caller: "admin", Method: post, Path: "/v1/runs/run_split_raw/rerun",
		Body: `{}`, WantCode: http.StatusAccepted, WantListings: 1, WantRawListed: true,
	}, { // Test 9: An operator who manages a template cannot save a split of a path into it.
		Entry: "template save", Caller: "operator", Method: put, Path: "/v1/templates/tpl_managed",
		Body: splitRawTemplate, WantCode: http.StatusForbidden,
	}, { // Test 10: An agent cannot either.
		Entry: "template save", Caller: "agent", Method: put, Path: "/v1/templates/tpl_managed",
		Body: splitRawTemplate, WantCode: http.StatusForbidden,
	}, { // Test 11: An admin can, and the webhook on the template then lists the path.
		Entry: "template save", Caller: "admin", Method: put, Path: "/v1/templates/tpl_managed",
		Body: splitRawTemplate, WantCode: http.StatusOK, WantListings: 1, WantRawListed: true,
	}, { // Test 12: The operator who manages it can save a split of a stored inventory.
		Entry: "template save", Caller: "operator", Method: put, Path: "/v1/templates/tpl_managed",
		Body: splitStoredTemplate, WantCode: http.StatusOK, WantListings: 1,
	}, { // Test 13: An operator's launch of a template that splits a path is refused.
		Entry: "template launch", Caller: "operator", Method: post,
		Path: "/v1/templates/tpl_split/launch", Body: `{}`, WantCode: http.StatusForbidden,
	}, { // Test 14: An agent's is refused.
		Entry: "template launch", Caller: "agent", Method: post,
		Path: "/v1/templates/tpl_split/launch", Body: `{}`, WantCode: http.StatusForbidden,
	}, { // Test 15: An admin's lists the path.
		Entry: "template launch", Caller: "admin", Method: post,
		Path: "/v1/templates/tpl_split/launch", Body: `{}`,
		WantCode: http.StatusAccepted, WantListings: 1, WantRawListed: true,
	}, { // Test 16: An operator's launch of a template that splits a stored inventory works.
		Entry: "template launch", Caller: "operator", Method: post,
		Path: "/v1/templates/tpl_stored/launch", Body: `{}`,
		WantCode: http.StatusAccepted, WantListings: 1,
	}, { // Test 17: A launch that names a stored inventory lists that one in place of the path.
		Entry: "template launch", Caller: "operator", Method: post,
		Path: "/v1/templates/tpl_split/launch", Body: `{"inventory_id":"inv_fleet"}`,
		WantCode: http.StatusAccepted, WantListings: 1,
	}, { // Test 18: An operator cannot save a schedule that splits a path, so none fires.
		Entry: "schedule save and fire", Caller: "operator", Method: post, Path: "/v1/schedules",
		Body: splitRawSchedule, WantCode: http.StatusForbidden,
	}, { // Test 19: An agent cannot either.
		Entry: "schedule save and fire", Caller: "agent", Method: post, Path: "/v1/schedules",
		Body: splitRawSchedule, WantCode: http.StatusForbidden,
	}, { // Test 20: An admin's is saved and lists the path when it fires.
		Entry: "schedule save and fire", Caller: "admin", Method: post, Path: "/v1/schedules",
		Body: splitRawSchedule, WantCode: http.StatusCreated, WantListings: 1, WantRawListed: true,
	}, { // Test 21: An operator cannot schedule the template that splits a path.
		Entry: "schedule save and fire", Caller: "operator", Method: post, Path: "/v1/schedules",
		Body: splitTemplateSchedule, WantCode: http.StatusForbidden,
	}, { // Test 22: An admin can, and it lists the path when it fires.
		Entry: "schedule save and fire", Caller: "admin", Method: post, Path: "/v1/schedules",
		Body:     splitTemplateSchedule,
		WantCode: http.StatusCreated, WantListings: 1, WantRawListed: true,
	}, { // Test 23: An operator cannot edit a schedule into a split of a path.
		Entry: "schedule save and fire", Caller: "operator", Method: put,
		Path: "/v1/schedules/sch_plain", Body: splitRawSchedule, WantCode: http.StatusForbidden,
	}, { // Test 24: An admin can, and it lists the path when it fires.
		Entry: "schedule save and fire", Caller: "admin", Method: put,
		Path: "/v1/schedules/sch_plain", Body: splitRawSchedule,
		WantCode: http.StatusOK, WantListings: 1, WantRawListed: true,
	}, { // Test 25: An operator cannot put a webhook on the template that splits a path.
		Entry: "trigger save and fire", Caller: "operator", Method: post, Path: "/v1/triggers",
		Body: splitRawTrigger, WantCode: http.StatusForbidden,
	}, { // Test 26: An agent cannot either.
		Entry: "trigger save and fire", Caller: "agent", Method: post, Path: "/v1/triggers",
		Body: splitRawTrigger, WantCode: http.StatusForbidden,
	}, { // Test 27: An admin can, and a delivery to it lists the path.
		Entry: "trigger save and fire", Caller: "admin", Method: post, Path: "/v1/triggers",
		Body: splitRawTrigger, WantCode: http.StatusCreated, WantListings: 1, WantRawListed: true,
	}, { // Test 28: An operator's workflow step names the path and lists nothing.
		Entry: "workflow node", Caller: "operator", Method: post, Path: "/v1/pipelines",
		Body: splitRawWorkflow, WantCode: http.StatusAccepted,
	}, { // Test 29: An agent's lists nothing.
		Entry: "workflow node", Caller: "agent", Method: post, Path: "/v1/pipelines",
		Body: splitRawWorkflow, WantCode: http.StatusAccepted,
	}, { // Test 30: An admin's lists nothing either, since a step cannot split.
		Entry: "workflow node", Caller: "admin", Method: post, Path: "/v1/pipelines",
		Body: splitRawWorkflow, WantCode: http.StatusAccepted,
	}, { // Test 31: An operator's launch of a saved workflow whose step names the path lists nothing.
		Entry: "workflow node", Caller: "operator", Method: post,
		Path: "/v1/templates/tpl_flow/launch", Body: `{}`, WantCode: http.StatusAccepted,
	}, { // Test 32: Shards on a saved workflow are refused as malformed, for an admin too.
		Entry: "workflow node", Caller: "admin", Method: put, Path: "/v1/templates/tpl_managed",
		Body: splitSteppedTemplate, WantCode: http.StatusBadRequest,
	}, { // Test 33: An agent proposing the template that splits a path is refused.
		Entry: "MCP", Caller: "agent", Tool: "propose_run", Body: `{"template_id":"tpl_split"}`,
		WantCode: http.StatusForbidden,
	}, { // Test 34: An operator's token behind the same tool is refused.
		Entry: "MCP", Caller: "operator", Tool: "propose_run", Body: `{"template_id":"tpl_split"}`,
		WantCode: http.StatusForbidden,
	}, { // Test 35: An agent proposing the template that splits a stored inventory is taken.
		Entry: "MCP", Caller: "agent", Tool: "propose_run", Body: `{"template_id":"tpl_stored"}`,
		WantCode: http.StatusAccepted, WantListings: 1,
	}, { // Test 36: An agent composing a run of a stored inventory with a limit lists nothing.
		Entry: "MCP", Caller: "agent", Tool: "propose_adhoc_run",
		Body:     `{"playbook":"site.yml","inventory_id":"inv_fleet","limit":"a,b"}`,
		WantCode: http.StatusAccepted,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s by %s", testNum, test.Entry, test.Caller), func(t *testing.T) {
			t.Parallel()
			s := newPathSplitServer(t)
			code, reply := s.send(t, s.token(t, test.Caller), test.Tool, test.Method, test.Path,
				test.Body)
			if code != test.WantCode {
				t.Errorf("%s by %s = %d, want %d: %s", test.Entry, test.Caller, code,
					test.WantCode, reply)
			}
			s.fireLaunchers(t, reply)
			listings, rawListed := s.Lister.listed(splitRawPath)
			if diff := cmp.Diff(test.WantListings, listings); diff != "" {
				t.Errorf("host listings mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantRawListed, rawListed); diff != "" {
				t.Errorf("caller's path listed mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSplitLimitThatNamesAFileIsRefusedAtEveryEntry pins that a limit holding an at sign is turned
// away where it is handed in, and never reaches a listing.
//
// Ansible reads what follows an at sign in a limit as a file of host patterns and opens it, and it
// does so for a sign after a comma, a colon, a space, either bracket, or a control character. A
// split hands the limit to the listing it does before the run exists, so the file was opened for
// whoever could submit. The limit is refused whole when it holds the sign anywhere. Every request
// here is an admin's, so the role rule for a split of a path plays no part.
//
//nolint:funlen // Test function.
func TestSplitLimitThatNamesAFileIsRefusedAtEveryEntry(t *testing.T) {
	t.Parallel()
	entries := []struct {
		// Name labels the entry.
		Name string
		// Method is the request method.
		Method string
		// Path is the request path.
		Path string
		// Body is the request body, with the encoded limit in place of the verb.
		Body string
		// Taken is the status an allowed limit answers.
		Taken int
	}{
		{Name: "submit", Method: http.MethodPost, Path: "/v1/runs", Taken: http.StatusAccepted,
			Body: `{"playbook":"site.yml","inventory":"hosts.ini","limit":%s}`},
		{Name: "split submit", Method: http.MethodPost, Path: "/v1/runs",
			Taken: http.StatusAccepted,
			Body:  `{"playbook":"site.yml","inventory_id":"inv_fleet","shards":2,"limit":%s}`},
		{Name: "template create", Method: http.MethodPost, Path: "/v1/templates",
			Taken: http.StatusCreated,
			Body:  `{"name":"limited","playbook":"site.yml","inventory":"hosts.ini","limit":%s}`},
		{Name: "template save", Method: http.MethodPut, Path: "/v1/templates/tpl_managed",
			Taken: http.StatusOK,
			Body:  `{"name":"deploy","playbook":"site.yml","inventory":"hosts.ini","limit":%s}`},
		{Name: "template launch", Method: http.MethodPost, Path: "/v1/templates/tpl_stored/launch",
			Taken: http.StatusAccepted, Body: `{"limit":%s}`},
		{Name: "composed inventory", Method: http.MethodPost, Path: "/v1/inventories",
			Taken: http.StatusCreated,
			Body: `{"name":"east","kind":"constructed","input_inventory_ids":["inv_fleet"],` +
				`"limit":%s}`},
	}
	tests := []struct {
		// Limit is the limit a caller sends.
		Limit string
		// WantRefused is whether every entry turns it away.
		WantRefused bool
	}{{ // Test 0: A file after a comma, behind a host that matches nothing.
		Limit: "nosuchhost,@/etc/shells", WantRefused: true,
	}, { // Test 1: A file after a colon.
		Limit: "web:@/etc/shells", WantRefused: true,
	}, { // Test 2: A file after a comma, the path absent.
		Limit: "web01,@/nonexistent/x", WantRefused: true,
	}, { // Test 3: A file after a space.
		Limit: "web01 @/nonexistent/x", WantRefused: true,
	}, { // Test 4: A file after a closing bracket.
		Limit: "web01]@/nonexistent/x", WantRefused: true,
	}, { // Test 5: A file after an opening bracket.
		Limit: "web01[@/nonexistent/x", WantRefused: true,
	}, { // Test 6: A file behind U+001C.
		Limit: "\x1c@/nonexistent/x", WantRefused: true,
	}, { // Test 7: Plain hosts are taken.
		Limit: "web01,web02",
	}, { // Test 8: A group pattern is taken.
		Limit: "web:!canary:&east",
	}}
	for testNum, test := range tests {
		for _, entry := range entries {
			t.Run(fmt.Sprintf("test %d %s", testNum, entry.Name), func(t *testing.T) {
				t.Parallel()
				s := newPathSplitServer(t)
				limit, err := json.Marshal(test.Limit)
				if err != nil {
					t.Fatalf("json.Marshal(%q) error = %v", test.Limit, err)
				}
				want := entry.Taken
				if test.WantRefused {
					want = http.StatusBadRequest
				}
				code, reply := s.call(s.Admin, entry.Method, entry.Path,
					fmt.Sprintf(entry.Body, limit))
				if code != want {
					t.Errorf("%s with limit %q = %d, want %d: %s", entry.Name, test.Limit, code,
						want, reply)
				}
				if found := s.Lister.fileLimits(); len(found) != 0 {
					t.Errorf("a listing was handed the limits %q", found)
				}
			})
		}
	}
}

// TestSplitRerunOfAStoredLimitThatNamesAFileIsRefused pins that a run stored with such a limit
// before the rule existed cannot be sent again: a rerun reads its limit from the stored run, so
// nothing the caller sends is there to refuse.
func TestSplitRerunOfAStoredLimitThatNamesAFileIsRefused(t *testing.T) {
	t.Parallel()
	s := newPathSplitServer(t)
	code, reply := s.call(s.Admin, http.MethodPost, "/v1/runs/run_limit_file/rerun", `{}`)
	if code != http.StatusConflict {
		t.Errorf("rerun of a run whose limit names a file = %d, want 409: %s", code, reply)
	}
	if listings, _ := s.Lister.listed(splitRawPath); listings != 0 {
		t.Errorf("the rerun asked for %d listings, want none", listings)
	}
}

// TestPathSplitSaveHandlersAskTheCaller calls the save handlers the role gate keeps for admins
// directly, with a caller below admin in the request, and pins that each one asks for itself.
//
// Schedule and trigger writes and template creation are admin-only at the gate today, so through
// the router a refusal there shows the gate and not the handler. The handlers are what stays true
// if that gate is ever widened, the way it already is for a caller who manages a template.
//
//nolint:funlen // Test function.
func TestPathSplitSaveHandlersAskTheCaller(t *testing.T) {
	t.Parallel()
	operator := &Actor{UserID: "user_lead", Role: user.RoleOperator, Name: "dev-lead"}
	agent := &Actor{UserID: "user_lead", Role: user.RoleOperator, Name: "release-agent",
		Agent: true, Type: "agent"}
	admin := &Actor{UserID: "user_admin", Role: user.RoleAdmin, Name: "approver"}
	const plainSchedule = `{"name":"nightly","cron":"0 2 * * *","playbook":"site.yml",` +
		`"inventory":"hosts.ini"}`
	tests := []struct {
		// Handler names the save handler called: schedule create, schedule update, trigger
		// create, or template create.
		Handler string
		// Actor is the caller in the request, nil on an install with no accounts.
		Actor *Actor
		// Body is the request body.
		Body string
		// WantCode is the status the handler answers.
		WantCode int
	}{{ // Test 0: An operator's schedule that splits a path is refused.
		Handler: "schedule create", Actor: operator, Body: splitRawSchedule,
		WantCode: http.StatusForbidden,
	}, { // Test 1: An agent's is refused.
		Handler: "schedule create", Actor: agent, Body: splitRawSchedule,
		WantCode: http.StatusForbidden,
	}, { // Test 2: An admin's is saved.
		Handler: "schedule create", Actor: admin, Body: splitRawSchedule,
		WantCode: http.StatusCreated,
	}, { // Test 3: An install with no accounts saves it.
		Handler: "schedule create", Body: splitRawSchedule, WantCode: http.StatusCreated,
	}, { // Test 4: An operator's schedule of the template that splits a path is refused.
		Handler: "schedule create", Actor: operator, Body: splitTemplateSchedule,
		WantCode: http.StatusForbidden,
	}, { // Test 5: An admin's is saved.
		Handler: "schedule create", Actor: admin, Body: splitTemplateSchedule,
		WantCode: http.StatusCreated,
	}, { // Test 6: An operator's plain schedule is saved, the control.
		Handler: "schedule create", Actor: operator, Body: plainSchedule,
		WantCode: http.StatusCreated,
	}, { // Test 7: An operator's edit of a schedule into a split of a path is refused.
		Handler: "schedule update", Actor: operator, Body: splitRawSchedule,
		WantCode: http.StatusForbidden,
	}, { // Test 8: An admin's is saved.
		Handler: "schedule update", Actor: admin, Body: splitRawSchedule, WantCode: http.StatusOK,
	}, { // Test 9: An operator's edit onto the template that splits a path is refused.
		Handler: "schedule update", Actor: operator, Body: splitTemplateSchedule,
		WantCode: http.StatusForbidden,
	}, { // Test 10: An admin's is saved.
		Handler: "schedule update", Actor: admin, Body: splitTemplateSchedule,
		WantCode: http.StatusOK,
	}, { // Test 11: An operator's webhook on the template that splits a path is refused.
		Handler: "trigger create", Actor: operator, Body: splitRawTrigger,
		WantCode: http.StatusForbidden,
	}, { // Test 12: An agent's is refused.
		Handler: "trigger create", Actor: agent, Body: splitRawTrigger,
		WantCode: http.StatusForbidden,
	}, { // Test 13: An admin's is saved.
		Handler: "trigger create", Actor: admin, Body: splitRawTrigger,
		WantCode: http.StatusCreated,
	}, { // Test 14: An operator's webhook on a plain template is saved, the control.
		Handler: "trigger create", Actor: operator,
		Body: `{"name":"on push","template_id":"tpl_managed"}`, WantCode: http.StatusCreated,
	}, { // Test 15: An operator's new template that splits a path is refused.
		Handler: "template create", Actor: operator, Body: splitRawTemplate,
		WantCode: http.StatusForbidden,
	}, { // Test 16: An admin's is saved.
		Handler: "template create", Actor: admin, Body: splitRawTemplate,
		WantCode: http.StatusCreated,
	}, { // Test 17: An operator's new template with shards and no inventory yet is saved.
		Handler: "template create", Actor: operator,
		Body: `{"name":"deploy","playbook":"site.yml","shards":2}`, WantCode: http.StatusCreated,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Handler), func(t *testing.T) {
			t.Parallel()
			db := agentHoldBackends()[0].Open(t)
			pathSplitSeed(t, db)
			var handler http.HandlerFunc
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.Body))
			switch test.Handler {
			case "schedule create":
				handler = createScheduleHandler(db.schedules, db.templates, nil, zap.NewNop())
			case "schedule update":
				handler = updateScheduleHandler(db.schedules, db.templates, nil, zap.NewNop())
				req.SetPathValue("id", "sch_plain")
			case "trigger create":
				handler = createTriggerHandler(db.triggers, db.templates, nil, nil, nil,
					zap.NewNop())
			case "template create":
				handler = createTemplateHandler(db.templates, nil, nil, nil, zap.NewNop())
			default:
				t.Fatalf("no handler named %q", test.Handler)
			}
			if test.Actor != nil {
				req = req.WithContext(context.WithValue(req.Context(), actorKey{}, *test.Actor))
			}
			rec := httptest.NewRecorder()
			handler(rec, req)
			if rec.Code != test.WantCode {
				t.Errorf("%s = %d, want %d: %s", test.Handler, rec.Code, test.WantCode,
					rec.Body.String())
			}
		})
	}
}
