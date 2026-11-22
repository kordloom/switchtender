package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// pagingBase is the creation time every paging fixture counts its runs from, so two stores built
// from the same plan hold byte-identical readable runs.
var pagingBase = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// pagingRun is one run in a paging fixture: its id, whether the org_a member may read it, its status,
// and its position in time.
type pagingRun struct {
	ID       string
	Readable bool
	Status   run.Status
	At       int
}

// pagingStore builds a memory store from a plan. A readable run belongs to proj_solo, which org_a
// owns, and a hidden one to proj_b, which org_b owns, so under orgOwnedFixture an org_a member reads
// exactly the readable ones.
func pagingStore(t *testing.T, plan []pagingRun) run.Store {
	t.Helper()
	store := run.NewMemStore()
	for _, p := range plan {
		project := "proj_b"
		if p.Readable {
			project = "proj_solo"
		}
		if err := store.Save(context.Background(), &run.Run{
			ID: p.ID, ProjectID: project, Status: p.Status, Command: "deploy",
			CreatedAt: pagingBase.Add(time.Duration(p.At) * time.Second),
		}); err != nil {
			t.Fatalf("save run %s: %v", p.ID, err)
		}
	}
	return store
}

// listRunsRaw drives a run list handler as the given actor and returns the raw body, failing the
// test on anything but 200.
func listRunsRaw(t *testing.T, handler http.Handler, ctx context.Context, query string) []byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/runs"+query, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/runs%s = %d, want 200 (body %s)", query, rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

// decodeRuns decodes a run list body.
func decodeRuns(t *testing.T, body []byte) listRunsResponse {
	t.Helper()
	var resp listRunsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode run list: %v (body %s)", err, body)
	}
	return resp
}

// countingRunStore counts the store pages a handler reads.
type countingRunStore struct {
	run.Store
	pages atomic.Int64
}

// ListPage counts the call and delegates.
func (c *countingRunStore) ListPage(ctx context.Context, f run.ListFilter, limit,
	offset int) ([]*run.Run, error) {
	c.pages.Add(1)
	return c.Store.ListPage(ctx, f, limit, offset)
}

// TestRunsPagingDoesNotCountHiddenRuns is the leak test. Two stores hold the same readable runs and
// differ only in hidden runs: the second holds five more that match the status filter, placed
// between readable ones. A restricted caller sends the same requests to both and must get the same
// bodies. Paging the store and filtering after gave a next_offset and a has_more that moved with the
// hidden runs, so a viewer could count another tenant's failed runs one status at a time.
func TestRunsPagingDoesNotCountHiddenRuns(t *testing.T) {
	t.Parallel()
	shared := []pagingRun{
		{"run_r1", true, run.StatusSucceeded, 1},
		{"run_h1", false, run.StatusFailed, 2},
		{"run_r2", true, run.StatusFailed, 3},
		{"run_r3", true, run.StatusSucceeded, 5},
		{"run_r4", true, run.StatusFailed, 7},
		{"run_h2", false, run.StatusSucceeded, 8},
		{"run_r5", true, run.StatusFailed, 9},
		{"run_r6", true, run.StatusSucceeded, 11},
	}
	extra := []pagingRun{
		{"run_x1", false, run.StatusFailed, 4},
		{"run_x2", false, run.StatusFailed, 6},
		{"run_x3", false, run.StatusFailed, 10},
		{"run_x4", false, run.StatusFailed, 12},
		{"run_x5", false, run.StatusFailed, 0},
	}
	authz := orgOwnedFixture(t)(true)
	few := listRunsHandler(pagingStore(t, shared), authz, zap.NewNop())
	more := listRunsHandler(pagingStore(t, append(append([]pagingRun{}, shared...), extra...)), authz,
		zap.NewNop())
	ctx := ctxActor("user_member_a", user.RoleViewer)

	// Six readable runs in all and three of them failed, so offsets run one past the end of each.
	tests := []struct {
		Name     string
		Filter   string
		Readable int
	}{ // Test 0: No filter.
		{"no filter", "", 6},
		// Test 1: The status the extra hidden runs share.
		{"status failed", "&status=failed", 3},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			var queries []string
			for k := 0; k <= test.Readable; k++ {
				queries = append(queries, fmt.Sprintf("?limit=1&offset=%d%s", k, test.Filter))
			}
			queries = append(queries, "?limit=200"+test.Filter)
			for _, q := range queries {
				a, b := listRunsRaw(t, few, ctx, q), listRunsRaw(t, more, ctx, q)
				if diff := cmp.Diff(string(a), string(b)); diff != "" {
					t.Errorf("GET /v1/runs%s differs with five more hidden runs in the store, so the "+
						"response counts runs this caller may not read (-fewer +more):\n%s", q, diff)
				}
			}
		})
	}
}

// TestRunsPagingCountsReadableRows pages one row at a time through thirty matching runs of which the
// caller reads every third. Each page holds the next readable run, next_offset is one past it in
// readable rows, and has_more turns false only on the last.
func TestRunsPagingCountsReadableRows(t *testing.T) {
	t.Parallel()
	var plan []pagingRun
	var want []string
	for i := range 30 {
		p := pagingRun{fmt.Sprintf("run_%02d", i), i%3 == 0, run.StatusSucceeded, i}
		plan = append(plan, p)
	}
	// Newest first.
	for i := 29; i >= 0; i-- {
		if plan[i].Readable {
			want = append(want, plan[i].ID)
		}
	}
	handler := listRunsHandler(pagingStore(t, plan), orgOwnedFixture(t)(true), zap.NewNop())
	ctx := ctxActor("user_member_a", user.RoleViewer)

	var got []string
	for k := range 10 {
		resp := decodeRuns(t, listRunsRaw(t, handler, ctx, fmt.Sprintf("?limit=1&offset=%d", k)))
		got = append(got, runIDs(resp.Runs)...)
		if resp.NextOffset != k+1 {
			t.Errorf("offset %d: next_offset = %d, want %d", k, resp.NextOffset, k+1)
		}
		if wantMore := k < 9; resp.HasMore != wantMore {
			t.Errorf("offset %d: has_more = %v, want %v", k, resp.HasMore, wantMore)
		}
		if resp.Partial {
			t.Errorf("offset %d: partial = true, want false: the walk never reached its bound", k)
		}
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("readable runs paged one at a time (-want +got):\n%s", diff)
	}
}

// TestRunsPagingStopsAtTheWalkBound sets the walk to two store pages of three rows. A list that
// reaches the bound says partial and not has_more, and one that ends inside it says neither.
func TestRunsPagingStopsAtTheWalkBound(t *testing.T) {
	t.Parallel()
	walk := runsWalk{pageSize: 3, pages: 2}
	authz := orgOwnedFixture(t)(true)
	ctx := ctxActor("user_member_a", user.RoleViewer)
	planOf := func(n int) []pagingRun {
		var plan []pagingRun
		for i := range n {
			plan = append(plan, pagingRun{fmt.Sprintf("run_%02d", i), i%3 == 0, run.StatusSucceeded, i})
		}
		return plan
	}
	tests := []struct {
		Name        string
		Runs        int
		WantRuns    int
		WantPartial bool
		WantPages   int64
	}{ // Test 0: Ten runs, the walk reads six and stops with two readable.
		{"bound reached", 10, 2, true, 2},
		// Test 1: Five runs, the second store page comes back short, so the walk found the end.
		{"end inside the bound", 5, 2, false, 2},
		// Test 2: Two runs, one short page.
		{"one short page", 2, 1, false, 1},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			store := &countingRunStore{Store: pagingStore(t, planOf(test.Runs))}
			handler := listRunsHandlerWalk(store, authz, zap.NewNop(), walk)
			body := listRunsRaw(t, handler, ctx, "?limit=200")
			resp := decodeRuns(t, body)
			if len(resp.Runs) != test.WantRuns {
				t.Errorf("runs = %v, want %d of them", runIDs(resp.Runs), test.WantRuns)
			}
			if resp.Partial != test.WantPartial {
				t.Errorf("partial = %v, want %v", resp.Partial, test.WantPartial)
			}
			if resp.HasMore {
				t.Error("has_more = true, want false: a walk that stopped at its bound does not know " +
					"that another readable run exists")
			}
			if got := store.pages.Load(); got != test.WantPages {
				t.Errorf("store pages read = %d, want %d", got, test.WantPages)
			}
			if !test.WantPartial && strings.Contains(string(body), `"partial"`) {
				t.Errorf("body carries a partial key when the list is whole: %s", body)
			}
		})
	}
}

// TestRunsPagingAdminReadsOneStorePage is the admin control. A caller grants place no restriction
// on keeps one store page per request, the store's own offsets, and a body with no partial key.
func TestRunsPagingAdminReadsOneStorePage(t *testing.T) {
	t.Parallel()
	var plan []pagingRun
	for i := range 6 {
		plan = append(plan, pagingRun{fmt.Sprintf("run_%02d", i), i%2 == 0, run.StatusSucceeded, i})
	}
	store := &countingRunStore{Store: pagingStore(t, plan)}
	handler := listRunsHandlerWalk(store, orgOwnedFixture(t)(true), zap.NewNop(),
		runsWalk{pageSize: 2, pages: 1})
	body := listRunsRaw(t, handler, ctxActor("user_root", user.RoleAdmin), "?limit=2&offset=2")
	resp := decodeRuns(t, body)
	if diff := cmp.Diff([]string{"run_03", "run_02"}, runIDs(resp.Runs), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("admin page (-want +got):\n%s", diff)
	}
	if resp.NextOffset != 4 || !resp.HasMore {
		t.Errorf("next_offset = %d, has_more = %v, want 4 and true", resp.NextOffset, resp.HasMore)
	}
	if got := store.pages.Load(); got != 1 {
		t.Errorf("store pages read = %d, want 1", got)
	}
	if strings.Contains(string(body), `"partial"`) {
		t.Errorf("an admin body carries a partial key: %s", body)
	}
}
