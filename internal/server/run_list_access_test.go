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
	// Both grant modes. What --strict-grants decides is the default for an object nobody granted;
	// an object that carries a grant is access controlled either way, which is what the by-id fetch
	// implements and what this guard is about. Pinning only the strict half let the two answers
	// disagree on the default install, where the list returned records the fetch refused.
	for _, strict := range []bool{true, false} {
		t.Run(fmt.Sprintf("strict=%v", strict), func(t *testing.T) {
			t.Parallel()
			runListAgreesWithFetch(t, strict)
		})
	}
}

// runListAgreesWithFetch is the body of the guard, run once per grant mode.
func runListAgreesWithFetch(t *testing.T, strict bool) {
	t.Helper()
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
		WithGrants(grants, strict), WithOrgs(orgs), WithProjects(projects), WithUsers(users)).Handler()

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

// TestListRunsCursorCountsReadableRows pins that a strict-grants reader pages through the rows it may
// read with no repeats and no gaps, and that the cursor counts those rows only.
//
// The read filter used to thin a store page after the store returned it. Advancing by the visible
// rows re-read the refused ones, so Load more repeated rows, and advancing by the store page sent the
// caller a cursor whose distance from the rows returned counted the runs it was refused.
func TestListRunsCursorCountsReadableRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runs := run.NewMemStore()
	users := user.NewMemStore()
	grants := grant.NewMemStore()
	orgs := org.NewMemStore()
	projects := project.NewMemStore()
	for _, p := range []*project.Project{
		{ID: "proj_a", Name: "a", RepoURL: "https://example.com/a.git"},
		{ID: "proj_b", Name: "b", RepoURL: "https://example.com/b.git"},
	} {
		if err := projects.Save(ctx, p); err != nil {
			t.Fatalf("Save() project error = %v", err)
		}
	}
	base := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		proj := "proj_a"
		if i%2 == 1 {
			proj = "proj_b"
		}
		if err := runs.Save(ctx, &run.Run{
			ID: "run_cursor_" + string(rune('0'+i)), Playbook: "p.yml", ProjectID: proj,
			Status: run.StatusSucceeded, CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("Save() run error = %v", err)
		}
	}
	u, err := user.New("pager", "pw", user.RoleOperator)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	if err := users.Save(ctx, u); err != nil {
		t.Fatalf("Save() user error = %v", err)
	}
	if err := grants.Save(ctx, &grant.Grant{
		ID: "grant_a", Subject: u.ID, Object: "proj_a", Access: grant.AccessUse,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() grant error = %v", err)
	}
	handler := New(runs, &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
		WithGrants(grants, true), WithOrgs(orgs), WithProjects(projects), WithUsers(users)).Handler()

	// Newest first the store holds 3 (proj_b), 2 (proj_a), 1 (proj_b), 0 (proj_a), and the caller
	// uses proj_a only.
	tests := []struct {
		Offset         int
		WantID         string
		WantNextOffset int
		WantHasMore    bool
	}{ // Test 0: The first readable run, with one more behind it.
		{0, "run_cursor_2", 1, true},
		// Test 1: The second and last readable run.
		{1, "run_cursor_0", 2, false},
	}
	for _, test := range tests {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/runs?limit=1&offset=%d", test.Offset), nil)
		req = req.WithContext(context.WithValue(req.Context(), actorKey{},
			Actor{UserID: u.ID, Role: user.RoleOperator, Name: "pager"}))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Runs []struct {
				ID string `json:"id"`
			} `json:"runs"`
			HasMore    bool `json:"has_more"`
			NextOffset int  `json:"next_offset"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Runs) != 1 || body.Runs[0].ID != test.WantID {
			t.Errorf("offset %d: runs = %+v, want only %s", test.Offset, body.Runs, test.WantID)
		}
		if body.NextOffset != test.WantNextOffset || body.HasMore != test.WantHasMore {
			t.Errorf("offset %d: next_offset = %d, has_more = %v, want %d and %v", test.Offset,
				body.NextOffset, body.HasMore, test.WantNextOffset, test.WantHasMore)
		}
	}
}
