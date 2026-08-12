package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/ai"
	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/user"
)

// The ids the fixture seeds. Everything prefixed victim belongs to another tenant, everything
// prefixed mine belongs to the caller these tests drive.
const (
	// wiringActor is the caller every case here drives: a real, non-admin user of one organization.
	wiringActor = "user_intruder"
	// wiringMyOrg is the organization the caller belongs to.
	wiringMyOrg = "org_intruder"
	// wiringTheirOrg is the organization the caller has no membership in.
	wiringTheirOrg = "org_victim"
	// bareVictimCommand is the inline script of the objectless victim run, a distinctive string so a
	// test can prove it did or did not leak into a response body.
	bareVictimCommand = "rm -rf /victim/data # VICTIM_SECRET_COMMAND"
)

// recordingRetrier is a Retrier that records which runs a relaunch reached, so a test can tell a
// refused request from one that quietly restarted somebody else's failed shards.
type recordingRetrier struct {
	// mu guards parents.
	mu sync.Mutex
	// parents holds the parent run ids the handler asked to relaunch, in order.
	parents []string
}

// RetryFailedShards records the parent id and returns a fresh run standing in for the relaunch.
func (r *recordingRetrier) RetryFailedShards(_ context.Context, parentID string) (*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.parents = append(r.parents, parentID)
	return &run.Run{ID: "run_relaunched", Playbook: "relaunch.yml", Status: run.StatusPending}, nil
}

// RelaunchFailedHosts records the run id and returns a fresh run standing in for the relaunch. The
// relaunch-failed route reaches this one, so a guard that stopped firing would show up here.
func (r *recordingRetrier) RelaunchFailedHosts(_ context.Context, runID, _ string) (*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.parents = append(r.parents, runID)
	return &run.Run{ID: "run_relaunched", Playbook: "relaunch.yml", Status: run.StatusPending}, nil
}

// reached returns a copy of the parent run ids the relaunch reached.
func (r *recordingRetrier) reached() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.parents)
}

// recordingApprover is an Approver that writes the decision onto the stored run, so a test reads
// the consequence of a decision out of the store rather than trusting a status code.
type recordingApprover struct {
	// runs is the store the decision is written to.
	runs run.Store
}

// Approve marks the run running, the state an approval releases it into.
func (a *recordingApprover) Approve(ctx context.Context, id string) (*run.Run, error) {
	return a.decide(ctx, id, run.StatusRunning)
}

// Reject marks the run rejected, recording the reason as its error.
func (a *recordingApprover) Reject(ctx context.Context, id, reason string) (*run.Run, error) {
	rn, err := a.decide(ctx, id, run.StatusRejected)
	if err != nil {
		return nil, err
	}
	rn.Error = reason
	return rn, nil
}

// decide writes status onto the stored run and returns it.
func (a *recordingApprover) decide(ctx context.Context, id string, status run.Status) (*run.Run, error) {
	rn, err := a.runs.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	rn.Status = status
	if err := a.runs.Save(ctx, rn); err != nil {
		return nil, err
	}
	return rn, nil
}

// wiringFixture is a real server over real stores, so a case drives the same route the mux serves
// and then reads back what the request did or did not change.
type wiringFixture struct {
	// Handler is the server's routed handler, the production wiring under test.
	Handler http.Handler
	// Runs holds the seeded runs.
	Runs run.Store
	// Projects holds the seeded projects.
	Projects project.Store
	// Inventories holds the seeded inventories.
	Inventories inventory.Store
	// Schedules holds the seeded schedules.
	Schedules schedule.Store
	// Sources holds the inventory sources, empty until a request creates one.
	Sources invsource.Store
	// Retrier records which runs a relaunch reached.
	Retrier *recordingRetrier
}

// newWiringFixture seeds two tenants and returns a server serving them under strict grants.
//
// The caller is a member of one organization and holds a use grant on that organization's project,
// inventory, credential, and template. They hold nothing at all on the other organization's
// objects, so every route reached with a victim id must refuse, and the same route reached with
// their own id must work. Each case builds its own fixture, so the stores it inspects carry only
// its own request.
func newWiringFixture(t *testing.T) *wiringFixture {
	t.Helper()
	ctx := context.Background()

	orgs := org.NewMemStore()
	for _, o := range []struct {
		ID   string
		User string
	}{{wiringTheirOrg, "user_victim"}, {wiringMyOrg, wiringActor}} {
		if err := orgs.Save(ctx, &org.Org{ID: o.ID, Name: o.ID}); err != nil {
			t.Fatalf("Save() org %s error = %v", o.ID, err)
		}
		if err := orgs.AddMember(ctx, o.ID, o.User, org.RoleMember); err != nil {
			t.Fatalf("AddMember() %s error = %v", o.ID, err)
		}
	}

	projects := project.NewMemStore()
	for _, p := range []*project.Project{
		{ID: "proj_victim", Name: "victim", RepoURL: "https://example.com/victim.git", OrgID: wiringTheirOrg},
		{ID: "proj_mine", Name: "mine", RepoURL: "https://example.com/mine.git", OrgID: wiringMyOrg},
	} {
		if err := projects.Save(ctx, p); err != nil {
			t.Fatalf("Save() project %s error = %v", p.ID, err)
		}
	}

	inventories := inventory.NewMemStore()
	for _, i := range []*inventory.Inventory{
		{ID: "inv_victim", Name: "victim fleet", Content: "[all]\nvictim-1\n", OrgID: wiringTheirOrg},
		{ID: "inv_mine", Name: "my fleet", Content: "[all]\nmine-1\n", OrgID: wiringMyOrg},
	} {
		if err := inventories.Save(ctx, i); err != nil {
			t.Fatalf("Save() inventory %s error = %v", i.ID, err)
		}
	}

	templates := template.NewMemStore()
	for _, tpl := range []*template.Template{
		{ID: "tpl_victim", Name: "victim deploy", Playbook: "victim.yml", OrgID: wiringTheirOrg},
		{ID: "tpl_mine", Name: "my deploy", Playbook: "mine.yml", OrgID: wiringMyOrg},
	} {
		if err := templates.Save(ctx, tpl); err != nil {
			t.Fatalf("Save() template %s error = %v", tpl.ID, err)
		}
	}

	schedules := schedule.NewMemStore()
	for _, sc := range []*schedule.Schedule{
		{ID: "sch_victim", Name: "victim nightly", Cron: "0 3 * * *", TemplateID: "tpl_victim", Enabled: true},
		{ID: "sch_mine", Name: "my nightly", Cron: "0 4 * * *", TemplateID: "tpl_mine", Enabled: true},
	} {
		if err := schedules.Save(ctx, sc); err != nil {
			t.Fatalf("Save() schedule %s error = %v", sc.ID, err)
		}
	}

	runs := run.NewMemStore()
	for _, rn := range []*run.Run{
		{
			ID: "run_victim", Playbook: "victim.yml", Inventory: "victim-hosts",
			ProjectID: "proj_victim", InventoryID: "inv_victim",
			Status: run.StatusPendingApproval, CreatedAt: time.Now(),
		},
		{
			ID: "run_mine", Playbook: "mine.yml", Inventory: "my-hosts",
			ProjectID: "proj_mine", InventoryID: "inv_mine",
			Status: run.StatusPendingApproval, CreatedAt: time.Now(),
		},
		// The objectless runs of B3: an inline script that names no project, inventory, or
		// credential, so there is nothing for the per-object grant check to filter on. Each is
		// scoped only by the org it was stamped with at submit.
		{
			ID: "run_victim_bare", Tool: run.ToolBash, Command: bareVictimCommand,
			OrgID: wiringTheirOrg, Status: run.StatusPendingApproval, CreatedAt: time.Now(),
		},
		{
			ID: "run_mine_bare", Tool: run.ToolBash, Command: "echo mine",
			OrgID: wiringMyOrg, Status: run.StatusPendingApproval, CreatedAt: time.Now(),
		},
	} {
		if err := runs.Save(ctx, rn); err != nil {
			t.Fatalf("Save() run %s error = %v", rn.ID, err)
		}
	}

	grants := grant.NewMemStore()
	for _, object := range []string{"proj_mine", "inv_mine", "cred_mine", "tpl_mine"} {
		if err := grants.Save(ctx, &grant.Grant{
			ID: "grant_" + object, Subject: wiringActor, Object: object,
			Access: grant.AccessUse, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("Save() grant on %s error = %v", object, err)
		}
	}

	sources := invsource.NewMemStore()
	retrier := &recordingRetrier{}
	explainer := ai.ProviderFunc(func(_ context.Context, _, _ string) (string, error) {
		return "an explanation", nil
	})
	srv := New(runs, &fakeSubmitter{}, zap.NewNop(),
		WithGrants(grants, true), WithOrgs(orgs), WithProjects(projects),
		WithInventories(inventories), WithTemplates(templates), WithSchedules(schedules),
		WithInventorySources(sources, nil), WithAudit(audit.NewMemStore()),
		WithApprover(&recordingApprover{runs: runs}), WithRetrier(retrier),
		WithAI(explainer))

	return &wiringFixture{
		Handler: srv.Handler(), Runs: runs, Projects: projects, Inventories: inventories,
		Schedules: schedules, Sources: sources, Retrier: retrier,
	}
}

// do drives the route as the foreign-organization actor and returns the recorded response.
func (f *wiringFixture) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), actorKey{},
		Actor{UserID: wiringActor, Role: user.RoleOperator, Name: "intruder"}))
	rec := httptest.NewRecorder()
	f.Handler.ServeHTTP(rec, req)
	return rec
}

// projectIDs returns the stored project ids, sorted, so a case can prove a refused write left the
// set of projects exactly as it found it.
func (f *wiringFixture) projectIDs(t *testing.T) []string {
	t.Helper()
	list, err := f.Projects.List(context.Background())
	if err != nil {
		t.Fatalf("List() projects error = %v", err)
	}
	ids := orgProjectIDs(list)
	slices.Sort(ids)
	return ids
}

// runStatus reads a run's stored status, so a case can prove a refused decision left it unchanged.
func (f *wiringFixture) runStatus(t *testing.T, id string) run.Status {
	t.Helper()
	rn, err := f.Runs.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get(%s) error = %v", id, err)
	}
	return rn.Status
}

// TestObjectlessRunIsScopedToItsOrg proves B3: a run that references no stored object is confined to
// the organization it was stamped with, so a grantless actor in another org cannot read it, cancel
// it, retry it, relaunch it, rerun it, approve it, reject it, export it, compare it, explain it, or
// stream it.
//
// authorizeAll over zero objects returns nil, so every run-scoped route authorized an objectless run
// vacuously: the foreign actor read the victim's full command and log, and a decision route settled
// or killed the victim's held run. Each case asserts the effect, that the body does not carry the
// victim's command and that a refused decision left the run's status unchanged, not merely a status
// code.
func TestObjectlessRunIsScopedToItsOrg(t *testing.T) {
	t.Parallel()

	// readRoutes must all refuse the foreign caller and never render the victim's command.
	readRoutes := []struct {
		// Name identifies the route.
		Name string
		// Method is the HTTP method.
		Method string
		// Path is the route reached against the objectless victim run.
		Path string
	}{
		{"get", http.MethodGet, "/v1/runs/run_victim_bare"},
		{"logs", http.MethodGet, "/v1/runs/run_victim_bare/logs"},
		{"events", http.MethodGet, "/v1/runs/run_victim_bare/events"},
		{"stream", http.MethodGet, "/v1/runs/run_victim_bare/stream"},
		{"evidence", http.MethodGet, "/v1/runs/run_victim_bare/evidence"},
		{"compare", http.MethodGet, "/v1/runs/run_victim_bare/compare?with=run_mine_bare"},
		{"explain", http.MethodPost, "/v1/runs/run_victim_bare/explain"},
	}
	for _, rt := range readRoutes {
		t.Run("read "+rt.Name, func(t *testing.T) {
			t.Parallel()
			f := newWiringFixture(t)
			// The stream is an open-ended SSE response, so the request carries a short deadline: a
			// refusal answers 403 at once, while a route that wrongly admits the foreign caller
			// writes its 200 status line and then unblocks on the deadline, which the code check
			// below catches as the leak it is.
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			req := httptest.NewRequest(rt.Method, rt.Path, nil)
			req = req.WithContext(context.WithValue(ctx, actorKey{},
				Actor{UserID: wiringActor, Role: user.RoleOperator, Name: "intruder"}))
			rec := httptest.NewRecorder()
			f.Handler.ServeHTTP(rec, req)
			if rec.Code < 400 {
				t.Errorf("%s answered %d, want a refusal of another tenant's objectless run",
					rt.Name, rec.Code)
			}
			if strings.Contains(rec.Body.String(), "VICTIM_SECRET_COMMAND") {
				t.Errorf("%s leaked the victim's command:\n%s", rt.Name, rec.Body.String())
			}
		})
	}

	// decisionRoutes must all refuse the foreign caller and leave the run held for approval.
	decisionRoutes := []struct {
		// Name identifies the route.
		Name string
		// Path is the route reached against the objectless victim run.
		Path string
		// Body is the request body, if any.
		Body string
		// WantReached reports whether the recording retrier should have been reached.
		WantReached bool
	}{
		{"cancel", "/v1/runs/run_victim_bare/cancel", "", false},
		{"approve", "/v1/runs/run_victim_bare/approve", "", false},
		{"reject", "/v1/runs/run_victim_bare/reject", "", false},
		{"retry", "/v1/runs/run_victim_bare/retry", "", false},
		{"relaunch", "/v1/runs/run_victim_bare/relaunch-failed", "", false},
		{"rerun", "/v1/runs/run_victim_bare/rerun", "", false},
	}
	for _, rt := range decisionRoutes {
		t.Run("decide "+rt.Name, func(t *testing.T) {
			t.Parallel()
			f := newWiringFixture(t)
			rec := f.do(t, http.MethodPost, rt.Path, rt.Body)
			if rec.Code < 400 {
				t.Errorf("%s answered %d, want a refusal", rt.Name, rec.Code)
			}
			if got := f.runStatus(t, "run_victim_bare"); got != run.StatusPendingApproval {
				t.Errorf("%s moved the victim's run to %q, want it still held for approval",
					rt.Name, got)
			}
			if reached := len(f.Retrier.reached()) > 0; reached != rt.WantReached {
				t.Errorf("%s reached=%v, want %v", rt.Name, reached, rt.WantReached)
			}
		})
	}

	// The runs list is derived from every run on the install, so it must drop the foreign
	// objectless run while keeping the caller's own. Fetching one and listing many decide the same
	// way, so a run hidden by id is hidden here too.
	t.Run("list excludes the foreign objectless run", func(t *testing.T) {
		t.Parallel()
		f := newWiringFixture(t)
		rec := f.do(t, http.MethodGet, "/v1/runs", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("list answered %d, want 200: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if strings.Contains(body, "run_victim_bare") || strings.Contains(body, "VICTIM_SECRET_COMMAND") {
			t.Errorf("list leaked another tenant's objectless run:\n%s", body)
		}
		if !strings.Contains(body, "run_mine_bare") {
			t.Errorf("list dropped the caller's own objectless run:\n%s", body)
		}
	})

	// The owner reaches the same routes: the confinement isolates a tenant, it does not lock the
	// run away from its own organization.
	t.Run("owner reads own objectless run", func(t *testing.T) {
		t.Parallel()
		f := newWiringFixture(t)
		req := httptest.NewRequest(http.MethodGet, "/v1/runs/run_victim_bare", nil)
		req = req.WithContext(context.WithValue(req.Context(), actorKey{},
			Actor{UserID: "user_victim", Role: user.RoleOperator, Name: "victim"}))
		rec := httptest.NewRecorder()
		f.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("owner get answered %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "run_victim_bare") {
			t.Errorf("owner get did not return the run:\n%s", rec.Body.String())
		}
	})
}

// TestEvidenceExportRefusesAForeignRun proves the evidence route authorizes the run before it
// renders it.
//
// The dossier is the whole run: its command line, its extra vars, the hosts it touched, and the
// audit entries around it, in one self-contained document. Nothing else on the route asks who is
// asking, so with the check gone the export answers any caller with another tenant's run in full.
func TestEvidenceExportRefusesAForeignRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name describes the run being exported.
		Name string
		// RunID is the run whose evidence is requested.
		RunID string
		// WantOK reports whether the export should be produced.
		WantOK bool
	}{{ // Test 0: Another organization's run, which the caller holds nothing on.
		Name: "foreign run", RunID: "run_victim", WantOK: false,
	}, { // Test 1: The caller's own run, which must still export.
		Name: "own run", RunID: "run_mine", WantOK: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			f := newWiringFixture(t)
			rec := f.do(t, http.MethodGet, "/v1/runs/"+test.RunID+"/evidence", "")
			body := rec.Body.String()
			if test.WantOK {
				if rec.Code != http.StatusOK {
					t.Fatalf("own evidence answered %d, want 200: %s", rec.Code, body)
				}
				if !strings.Contains(body, test.RunID) {
					t.Errorf("own evidence does not name %s, so the export is not the run's own",
						test.RunID)
				}
				return
			}
			if rec.Code < 400 {
				t.Errorf("foreign evidence answered %d, want a refusal", rec.Code)
			}
			for _, leaked := range []string{test.RunID, "victim.yml", "victim-hosts", "<!DOCTYPE"} {
				if strings.Contains(body, leaked) {
					t.Errorf("the response carries %q, so another tenant's dossier was rendered: %s",
						leaked, body)
				}
			}
		})
	}
}

// TestRunDecisionsRefuseAForeignRun proves approving, rejecting, and relaunching a run authorize
// the run first.
//
// Each of the three acts on somebody else's production work: two settle a held run onto real hosts
// or kill it, and the third restarts its failed shards. The consequence is read out of the store,
// so a refusal that still moved the run would fail here.
func TestRunDecisionsRefuseAForeignRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name describes the decision.
		Name string
		// Action is the route segment after the run id.
		Action string
		// RunID is the run the decision is aimed at.
		RunID string
		// WantStatus is the run's status in the store afterwards.
		WantStatus run.Status
		// WantRelaunched is the set of run ids the relaunch reached.
		WantRelaunched []string
		// WantOK reports whether the decision should be carried out.
		WantOK bool
	}{{ // Test 0: Releasing another organization's held run onto its hosts.
		Name: "approve a foreign run", Action: "approve", RunID: "run_victim",
		WantStatus: run.StatusPendingApproval,
	}, { // Test 1: Killing another organization's held run.
		Name: "reject a foreign run", Action: "reject", RunID: "run_victim",
		WantStatus: run.StatusPendingApproval,
	}, { // Test 2: Relaunching the failed shards of another organization's run.
		Name: "relaunch a foreign run", Action: "retry", RunID: "run_victim",
		WantStatus: run.StatusPendingApproval,
	}, { // Test 3: Approving the caller's own held run still releases it.
		Name: "approve an own run", Action: "approve", RunID: "run_mine",
		WantStatus: run.StatusRunning, WantOK: true,
	}, { // Test 4: Rejecting the caller's own held run still denies it.
		Name: "reject an own run", Action: "reject", RunID: "run_mine",
		WantStatus: run.StatusRejected, WantOK: true,
	}, { // Test 5: Relaunching the caller's own run still reaches the retrier.
		Name: "relaunch an own run", Action: "retry", RunID: "run_mine",
		WantStatus: run.StatusPendingApproval, WantRelaunched: []string{"run_mine"}, WantOK: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			f := newWiringFixture(t)
			rec := f.do(t, http.MethodPost, "/v1/runs/"+test.RunID+"/"+test.Action, `{"reason":"no"}`)
			if ok := rec.Code < 400; ok != test.WantOK {
				t.Errorf("%s answered %d: %s", test.Name, rec.Code, rec.Body.String())
			}
			got, err := f.Runs.Get(context.Background(), test.RunID)
			if err != nil {
				t.Fatalf("Get(%s) error = %v", test.RunID, err)
			}
			if got.Status != test.WantStatus {
				t.Errorf("%s is %q after the request, want %q: the decision was carried out on a "+
					"run the caller holds nothing on", test.RunID, got.Status, test.WantStatus)
			}
			if diff := cmp.Diff(test.WantRelaunched, f.Retrier.reached(),
				cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("relaunched runs mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestProjectCreateRefusesUnreachableRepoAndBorrowedObjects proves creating a project validates its
// remote and authorizes what it names, before anything is stored.
//
// A project's remote is fetched by the server, so an unvalidated URL turns the create route into a
// request forger pointed at the cloud metadata endpoint. Its credential is the identity the clone
// authenticates as, and its organization decides who inherits access, so both are the caller's to
// choose only when they already hold them.
func TestProjectCreateRefusesUnreachableRepoAndBorrowedObjects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name describes the create being attempted.
		Name string
		// Body is the JSON request.
		Body string
		// WantIDs is the sorted set of stored project ids afterwards.
		WantIDs []string
		// WantOK reports whether the project should be created.
		WantOK bool
	}{{ // Test 0: The cloud metadata endpoint, which the server would fetch on the caller's behalf.
		Name:    "cloud metadata endpoint",
		Body:    `{"name":"probe","repo_url":"http://169.254.169.254/latest/meta-data/"}`,
		WantIDs: []string{"proj_mine", "proj_victim"},
	}, { // Test 1: A credential the caller was never granted, used as the clone identity.
		Name:    "borrowed credential",
		Body:    `{"name":"borrow","repo_url":"https://example.com/x.git","credential_id":"cred_victim"}`,
		WantIDs: []string{"proj_mine", "proj_victim"},
	}, { // Test 2: Planting a project inside an organization the caller does not belong to.
		Name:    "foreign organization",
		Body:    `{"name":"planted","repo_url":"https://example.com/x.git","org_id":"org_victim"}`,
		WantIDs: []string{"proj_mine", "proj_victim"},
	}, { // Test 3: The happy path the guards must not break.
		Name: "own credential in own organization",
		Body: `{"name":"ours","repo_url":"https://example.com/ours.git",` +
			`"credential_id":"cred_mine","org_id":"org_intruder"}`,
		WantOK: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			f := newWiringFixture(t)
			rec := f.do(t, http.MethodPost, "/v1/projects", test.Body)
			if ok := rec.Code < 400; ok != test.WantOK {
				t.Errorf("%s answered %d: %s", test.Name, rec.Code, rec.Body.String())
			}
			got := f.projectIDs(t)
			if test.WantOK {
				if len(got) != 3 {
					t.Errorf("stored projects are %v, want the seeded two plus the new one", got)
				}
				return
			}
			if diff := cmp.Diff(test.WantIDs, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("stored projects mismatch (-want +got):\n%s\na refused create wrote a "+
					"project anyway", diff)
			}
		})
	}
}

// TestInventoryUpdateCannotCrossTenants proves updating an inventory authorizes the credentials it
// names and both ends of an organization move.
//
// An inventory is the host list a run targets and it carries the credentials materialized for every
// run against it. Moving one into the caller's organization hands them somebody else's fleet;
// moving one out takes it away from the members who had it. The stored object is read back, so a
// refusal that still moved it fails here.
func TestInventoryUpdateCannotCrossTenants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name describes the update being attempted.
		Name string
		// ID is the inventory being updated.
		ID string
		// Body is the JSON request.
		Body string
		// WantName is the stored name afterwards.
		WantName string
		// WantOrgID is the stored owning organization afterwards.
		WantOrgID string
		// WantCredentialIDs are the stored credentials afterwards.
		WantCredentialIDs []string
		// WantOK reports whether the update should be applied.
		WantOK bool
	}{{ // Test 0: Attaching a credential the caller was never granted to their own inventory.
		Name: "borrowed credential", ID: "inv_mine",
		Body: `{"name":"renamed","content":"[all]\nmine-1\n","credential_ids":["cred_victim"],` +
			`"org_id":"org_intruder"}`,
		WantName: "my fleet", WantOrgID: wiringMyOrg,
	}, { // Test 1: Pushing their own inventory into an organization they do not belong to.
		Name: "into a foreign organization", ID: "inv_mine",
		Body:     `{"name":"renamed","content":"[all]\nmine-1\n","org_id":"org_victim"}`,
		WantName: "my fleet", WantOrgID: wiringMyOrg,
	}, { // Test 2: Pulling another organization's inventory into their own.
		Name: "out of a foreign organization", ID: "inv_victim",
		Body:     `{"name":"taken","content":"[all]\nvictim-1\n","org_id":"org_intruder"}`,
		WantName: "victim fleet", WantOrgID: wiringTheirOrg,
	}, { // Test 3: The happy path the guards must not break.
		Name: "own inventory and own credential", ID: "inv_mine",
		Body: `{"name":"renamed","content":"[all]\nmine-2\n","credential_ids":["cred_mine"],` +
			`"org_id":"org_intruder"}`,
		WantName: "renamed", WantOrgID: wiringMyOrg,
		WantCredentialIDs: []string{"cred_mine"}, WantOK: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			f := newWiringFixture(t)
			rec := f.do(t, http.MethodPut, "/v1/inventories/"+test.ID, test.Body)
			if ok := rec.Code < 400; ok != test.WantOK {
				t.Errorf("%s answered %d: %s", test.Name, rec.Code, rec.Body.String())
			}
			got, err := f.Inventories.Get(context.Background(), test.ID)
			if err != nil {
				t.Fatalf("Get(%s) error = %v", test.ID, err)
			}
			if got.Name != test.WantName {
				t.Errorf("stored name is %q, want %q", got.Name, test.WantName)
			}
			if got.OrgID != test.WantOrgID {
				t.Errorf("stored org is %q, want %q: the inventory changed tenant", got.OrgID,
					test.WantOrgID)
			}
			if diff := cmp.Diff(test.WantCredentialIDs, got.CredentialIDs,
				cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("stored credentials mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestScheduleUpdateAuthorizesStoredTemplateAndValidates proves updating a schedule authorizes the
// template already stored on it and refuses a schedule with nothing to fire.
//
// A schedule runs with nobody present, so the template it fires is authorized when the schedule is
// written. Checking only the body let a caller take one over by leaving template_id out: nothing was
// named, so nothing was checked, and the schedule was rewritten to run a playbook of their choosing
// on the owner's timetable. Validation is the other half: a schedule with no target is stored dead
// and silently stops firing.
func TestScheduleUpdateAuthorizesStoredTemplateAndValidates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name describes the update being attempted.
		Name string
		// ID is the schedule being updated.
		ID string
		// Body is the JSON request.
		Body string
		// WantName is the stored name afterwards.
		WantName string
		// WantCron is the stored cron expression afterwards.
		WantCron string
		// WantPlaybook is the stored playbook afterwards.
		WantPlaybook string
		// WantTemplateID is the stored template afterwards.
		WantTemplateID string
		// WantOK reports whether the update should be applied.
		WantOK bool
	}{{ // Test 0: Taking over another organization's schedule by naming no template.
		Name: "somebody else's schedule with the template left out", ID: "sch_victim",
		Body:     `{"name":"mine","cron":"0 5 * * *","playbook":"mine.yml"}`,
		WantName: "victim nightly", WantCron: "0 3 * * *", WantTemplateID: "tpl_victim",
	}, { // Test 1: Emptying their own schedule so it has nothing left to fire.
		Name: "a schedule with nothing to fire", ID: "sch_mine",
		Body:     `{"name":"blank","cron":"0 5 * * *"}`,
		WantName: "my nightly", WantCron: "0 4 * * *", WantTemplateID: "tpl_mine",
	}, { // Test 2: The happy path the guards must not break.
		Name: "own schedule firing an own template", ID: "sch_mine",
		Body:     `{"name":"renamed","cron":"0 6 * * *","template_id":"tpl_mine"}`,
		WantName: "renamed", WantCron: "0 6 * * *", WantTemplateID: "tpl_mine", WantOK: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			f := newWiringFixture(t)
			rec := f.do(t, http.MethodPut, "/v1/schedules/"+test.ID, test.Body)
			if ok := rec.Code < 400; ok != test.WantOK {
				t.Errorf("%s answered %d: %s", test.Name, rec.Code, rec.Body.String())
			}
			got, err := f.Schedules.Get(context.Background(), test.ID)
			if err != nil {
				t.Fatalf("Get(%s) error = %v", test.ID, err)
			}
			if got.Name != test.WantName || got.Cron != test.WantCron {
				t.Errorf("stored schedule is %q on %q, want %q on %q", got.Name, got.Cron,
					test.WantName, test.WantCron)
			}
			if got.Playbook != test.WantPlaybook {
				t.Errorf("stored playbook is %q, want %q: the schedule now runs somebody else's "+
					"timetable", got.Playbook, test.WantPlaybook)
			}
			if got.TemplateID != test.WantTemplateID {
				t.Errorf("stored template is %q, want %q", got.TemplateID, test.WantTemplateID)
			}
		})
	}
}

// TestSourceCreateAuthorizesWhatItReferences proves creating an inventory source authorizes the
// project its config comes from and the credential its plugin runs with.
//
// A source decides which hosts a run targets and carries the credential used to fetch them, so
// naming somebody else's project or credential borrows both. The source list and the inventory the
// route creates alongside it are read back, since the handler mints the inventory first and a
// refusal that leaves one behind is still a write.
func TestSourceCreateAuthorizesWhatItReferences(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name describes the create being attempted.
		Name string
		// Body is the JSON request.
		Body string
		// WantOK reports whether the source should be created.
		WantOK bool
	}{{ // Test 0: Sourcing the config from another organization's project.
		Name: "borrowed project",
		Body: `{"name":"borrowed","source":"aws_ec2.yml","project_id":"proj_victim"}`,
	}, { // Test 1: Running the plugin with another organization's credential.
		Name: "borrowed credential",
		Body: `{"name":"borrowed","source":"aws_ec2.yml","credential_id":"cred_victim"}`,
	}, { // Test 2: The happy path the guards must not break.
		Name:   "own project and own credential",
		Body:   `{"name":"ours","source":"aws_ec2.yml","project_id":"proj_mine","credential_id":"cred_mine"}`,
		WantOK: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			f := newWiringFixture(t)
			ctx := context.Background()
			rec := f.do(t, http.MethodPost, "/v1/inventory-sources", test.Body)
			if ok := rec.Code < 400; ok != test.WantOK {
				t.Errorf("%s answered %d: %s", test.Name, rec.Code, rec.Body.String())
			}
			sources, err := f.Sources.List(ctx)
			if err != nil {
				t.Fatalf("List() sources error = %v", err)
			}
			wantSources := 0
			if test.WantOK {
				wantSources = 1
			}
			if len(sources) != wantSources {
				t.Errorf("stored sources = %d, want %d: a refused create wrote a source that "+
					"reaches objects the caller holds nothing on", len(sources), wantSources)
			}
			invs, err := f.Inventories.List(ctx)
			if err != nil {
				t.Fatalf("List() inventories error = %v", err)
			}
			if len(invs) != 2+wantSources {
				t.Errorf("stored inventories = %d, want %d: the backing inventory was minted for a "+
					"source that was refused", len(invs), 2+wantSources)
			}
		})
	}
}
