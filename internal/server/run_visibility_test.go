package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// runIDs extracts the ids of a run slice in order.
func runIDs(rs []*run.Run) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

// TestReadableRunsOrgOwned proves run visibility is organization-aware: under strict grants a member
// of an org sees a run owned by that org through membership alone, while a member of another org does
// not. Resolving each run object's owning org with an empty string dropped every org-owned run from
// the list and every run-derived view.
func TestReadableRunsOrgOwned(t *testing.T) {
	t.Parallel()
	authz := orgOwnedFixture(t)(true)
	// proj_solo is owned by org_a, proj_b by org_b; neither carries an explicit grant, so visibility
	// rests on org membership alone.
	runs := []*run.Run{
		{ID: "run_a", ProjectID: "proj_solo"},
		{ID: "run_b", ProjectID: "proj_b"},
	}

	tests := []struct {
		Name   string
		Actor  string
		WantID []string
	}{ // Test 0: A member of org_a sees org_a's run, not org_b's.
		{"org_a member", "user_member_a", []string{"run_a"}},
		// Test 1: A member of org_b sees org_b's run, not org_a's.
		{"org_b member", "user_b", []string{"run_b"}},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			got, err := readableRuns(ctxActor(test.Actor, user.RoleViewer), authz, runs)
			if err != nil {
				t.Fatalf("readableRuns() error = %v", err)
			}
			if diff := cmp.Diff(test.WantID, runIDs(got), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("visible runs mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// listRunsFor drives the runs list handler as the given actor and decodes the response.
func listRunsFor(t *testing.T, store run.Store, authz *authorizer, actor, query string) listRunsResponse {
	t.Helper()
	handler := listRunsHandler(store, authz, zap.NewNop())
	req := httptest.NewRequest(http.MethodGet, "/v1/runs"+query, nil)
	req = req.WithContext(ctxActor(actor, user.RoleViewer))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list runs status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp listRunsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	return resp
}

// saveRun stores a run with a project owner and creation time so ordering and visibility are
// deterministic.
func saveRun(t *testing.T, store run.Store, id, projectID string, at time.Time) {
	t.Helper()
	if err := store.Save(context.Background(), &run.Run{
		ID: id, ProjectID: projectID, Status: run.StatusSucceeded, CreatedAt: at,
	}); err != nil {
		t.Fatalf("save run %s: %v", id, err)
	}
}

// TestListRunsHasMoreBeforeFilter proves HasMore reflects what the store returned, not the page after
// the read filter thinned it. A full store page with a row dropped by the filter must still report
// more, or later readable runs never page in.
func TestListRunsHasMoreBeforeFilter(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	// Three runs newest last; a page of two returns run_c and run_b, of which only run_c is readable
	// by an org_a member. run_a remains behind the page, so more genuinely follows.
	saveRun(t, store, "run_a", "proj_solo", base)
	saveRun(t, store, "run_b", "proj_b", base.Add(time.Second))
	saveRun(t, store, "run_c", "proj_solo", base.Add(2*time.Second))

	authz := orgOwnedFixture(t)(true)
	resp := listRunsFor(t, store, authz, "user_member_a", "?limit=2")

	if diff := cmp.Diff([]string{"run_c"}, runIDs(resp.Runs), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("visible runs mismatch (-want +got):\n%s", diff)
	}
	if !resp.HasMore {
		t.Error("HasMore = false, want true: the store returned a full page with a filtered row and " +
			"a further run exists, so more pages follow")
	}
}

// TestListRunsSummaryWithheld proves the install-wide status summary is withheld from a strict-grants
// viewer who can read no runs, the same aggregate-withholding the drift and task views do. Building it
// from the unfiltered store counts leaked how much activity the install had to a caller refused every
// run by name.
func TestListRunsSummaryWithheld(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	// Every run belongs to org_b, so an org_a member can read none of them.
	saveRun(t, store, "run_1", "proj_b", base)
	saveRun(t, store, "run_2", "proj_b", base.Add(time.Second))

	authz := orgOwnedFixture(t)(true)
	resp := listRunsFor(t, store, authz, "user_member_a", "")

	if len(resp.Runs) != 0 {
		t.Fatalf("visible runs = %v, want none", runIDs(resp.Runs))
	}
	if diff := cmp.Diff(runSummary{}, resp.Summary); diff != "" {
		t.Errorf("summary leaked install totals to a viewer who reads no runs (-want +got):\n%s", diff)
	}
}

// TestListRunsSummaryShownToReader confirms the summary is still shown to a caller who can read at
// least one run, so the withholding does not over-reach and blank the cards for a legitimate viewer.
func TestListRunsSummaryShownToReader(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	saveRun(t, store, "run_1", "proj_solo", base)
	saveRun(t, store, "run_2", "proj_b", base.Add(time.Second))

	authz := orgOwnedFixture(t)(true)
	resp := listRunsFor(t, store, authz, "user_member_a", "")

	if resp.Summary.Total == 0 {
		t.Error("summary total = 0, want the install totals for a viewer who can read a run")
	}
}
