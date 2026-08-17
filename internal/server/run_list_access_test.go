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
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestTheRunListAgreesWithFetchingARunByID covers a disclosure the list handler was written to prevent
// and did not.
//
// Fetching a run by id requires use on every object it touched. Listing runs filtered on read, and any
// grant satisfies read, so an explicit read grant on one of a run's objects put that whole run in the
// list, with its command, its extra vars, and the credentials it named, while fetching the same run by
// id answered 403. The list body is where the leak lands: a run's extra vars routinely carry survey
// answers and inline values, and the list masks only notification targets.
//
// Read grants are ordinary: the grant API accepts them. So this is reachable by configuring exactly what
// the API offers, and the list handler's own comment says a caller refused a run by id must not be able
// to read it by listing instead.
func TestTheRunListAgreesWithFetchingARunByID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const secret = "EXTRA-VAR-MUST-NOT-BE-LISTED"

	users := user.NewMemStore()
	orgs := org.NewMemStore()
	grants := grant.NewMemStore()
	projects := project.NewMemStore()
	runs := run.NewMemStore()

	// A project owned by an organization the caller does not belong to.
	if err := orgs.Save(ctx, &org.Org{ID: "org_owner", Name: "owner"}); err != nil {
		t.Fatalf("Save() org error = %v", err)
	}
	if err := projects.Save(ctx, &project.Project{
		ID: "proj_p", Name: "prod", RepoURL: "https://example.com/p.git", OrgID: "org_owner",
	}); err != nil {
		t.Fatalf("Save() project error = %v", err)
	}

	// A run that used it, carrying the kind of variable a survey fills in.
	if err := runs.Save(ctx, &run.Run{
		ID: "run_listed", Playbook: "site.yml", Inventory: "prod", ProjectID: "proj_p",
		Status: run.StatusSucceeded, CreatedAt: time.Now(), OrgID: "org_owner",
		Command:   "deploy",
		ExtraVars: map[string]any{"db_password": secret},
	}); err != nil {
		t.Fatalf("Save() run error = %v", err)
	}

	// The caller: a real non-admin with a read grant on the project and nothing else. No org membership.
	u, err := user.New("reader", "pw", user.RoleOperator)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	if err := users.Save(ctx, u); err != nil {
		t.Fatalf("Save() user error = %v", err)
	}
	if err := grants.Save(ctx, &grant.Grant{
		ID: "grant_read", Subject: u.ID, Object: "proj_p", Access: grant.AccessRead,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() grant error = %v", err)
	}

	handler := New(runs, &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
		WithGrants(grants, true), WithOrgs(orgs), WithProjects(projects), WithUsers(users)).Handler()

	as := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req = req.WithContext(context.WithValue(req.Context(), actorKey{},
			Actor{UserID: u.ID, Role: user.RoleOperator, Name: "reader"}))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// The by-id fetch is the boundary. Confirm it refuses, so the comparison below means something.
	byID := as(http.MethodGet, "/v1/runs/run_listed")
	if byID.Code < 400 {
		t.Fatalf("fetching the run by id = %d, want a refusal: the rest of this test compares the "+
			"list against that answer", byID.Code)
	}

	list := as(http.MethodGet, "/v1/runs")
	if list.Code != http.StatusOK {
		t.Fatalf("listing runs = %d, want 200 (body %s)", list.Code, list.Body.String())
	}
	body := list.Body.String()
	if strings.Contains(body, "run_listed") {
		t.Errorf("the list returns a run the by-id fetch refuses, so a read grant reads what use "+
			"guards:\n%s", body)
	}
	if strings.Contains(body, secret) {
		t.Errorf("the list returns the run's extra vars, which carry the values a survey filled in:\n%s",
			body)
	}
}
