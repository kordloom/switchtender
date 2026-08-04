package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// seedComparableRuns stores two finished template runs with host and task summaries, the newer
// one failing where the older succeeded, and one unrelated newer run from another template.
func seedComparableRuns(t *testing.T, store run.Store) {
	t.Helper()
	ctx := context.Background()
	at := func(min int) *time.Time {
		ts := time.Date(2026, 8, 4, 12, min, 0, 0, time.UTC)
		return &ts
	}
	runs := []*run.Run{
		{ID: "run_old", Playbook: "site.yml", Status: run.StatusSucceeded, Source: "template",
			SourceID: "tpl_1", CreatedAt: *at(0), StartedAt: at(0), EndedAt: at(2)},
		{ID: "run_other", Playbook: "other.yml", Status: run.StatusSucceeded, Source: "template",
			SourceID: "tpl_2", CreatedAt: *at(5), StartedAt: at(5), EndedAt: at(6)},
		{ID: "run_new", Playbook: "site.yml", Status: run.StatusFailed, Source: "template",
			SourceID: "tpl_1", CreatedAt: *at(10), StartedAt: at(10), EndedAt: at(13)},
	}
	summaries := map[string][]run.HostSummary{
		"run_old": {{Host: "web01", Worst: "ok", OK: 4}, {Host: "db01", Worst: "ok", OK: 2}},
		"run_new": {{Host: "web01", Worst: "failed", Failures: 1}, {Host: "db01", Worst: "ok", OK: 2}},
	}
	tasks := map[string][]run.TaskSummary{
		"run_old": {{Task: "deploy", Seconds: 10}},
		"run_new": {{Task: "deploy", Seconds: 35}},
	}
	// Summaries land while the run is still live, the way finalize writes them: a terminal run
	// fences its summaries against late overwrites.
	for _, rn := range runs {
		final := rn.Status
		rn.Status = run.StatusRunning
		if err := store.Save(ctx, rn); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		if sums := summaries[rn.ID]; len(sums) > 0 {
			if err := store.SaveHostSummary(ctx, rn.ID, sums); err != nil {
				t.Fatalf("SaveHostSummary() error = %v", err)
			}
		}
		if ts := tasks[rn.ID]; len(ts) > 0 {
			if err := store.SaveTaskSummary(ctx, rn.ID, ts); err != nil {
				t.Fatalf("SaveTaskSummary() error = %v", err)
			}
		}
		rn.Status = final
		if err := store.Save(ctx, rn); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
}

func TestRunCompare(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	seedComparableRuns(t, store)
	handler := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()

	get := func(t *testing.T, path string) (*httptest.ResponseRecorder, run.Comparison) {
		t.Helper()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var c run.Comparison
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
		}
		return rec, c
	}

	// with=prev resolves to the previous run of the same template, skipping the unrelated run
	// that sits between them in time.
	rec, c := get(t, "/v1/runs/run_new/compare?with=prev")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if c.B.ID != "run_old" {
		t.Fatalf("baseline = %s, want the previous run of the same template", c.B.ID)
	}
	if !c.SameSource || c.Totals.Broke != 1 || c.Totals.OK != 1 {
		t.Errorf("comparison = same_source %v totals %+v, want the broken web01 named", c.SameSource, c.Totals)
	}
	if c.DurationDeltaSeconds == nil || *c.DurationDeltaSeconds != 60 {
		t.Errorf("duration delta = %v, want the extra minute", c.DurationDeltaSeconds)
	}
	if len(c.Tasks) != 1 || c.Tasks[0].DeltaSeconds != 25 {
		t.Errorf("tasks = %+v, want deploy's 25s swing", c.Tasks)
	}

	// An explicit baseline works the same way.
	if rec, c = get(t, "/v1/runs/run_new/compare?with=run_old"); rec.Code != http.StatusOK ||
		c.B.ID != "run_old" {
		t.Errorf("explicit baseline status = %d baseline = %s", rec.Code, c.B.ID)
	}

	// The oldest run has nothing earlier: that is a named refusal, not an empty diff.
	if rec, _ = get(t, "/v1/runs/run_old/compare?with=prev"); rec.Code != http.StatusNotFound {
		t.Errorf("compare with no earlier run status = %d, want 404", rec.Code)
	}
	// Self-comparison is refused.
	if rec, _ = get(t, "/v1/runs/run_new/compare?with=run_new"); rec.Code != http.StatusBadRequest {
		t.Errorf("self comparison status = %d, want 400", rec.Code)
	}
	// An unknown baseline is a 404, not a crash.
	if rec, _ = get(t, "/v1/runs/run_new/compare?with=run_nope"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown baseline status = %d, want 404", rec.Code)
	}
}

func TestCompareDoesNotReadThroughTheBaseline(t *testing.T) {
	t.Parallel()
	authz := orgOwnedFixture(t)(true)
	store := run.NewMemStore()
	ctx := context.Background()
	at := func(min int) time.Time { return time.Date(2026, 8, 4, 12, min, 0, 0, time.UTC) }
	// The examined run lives in org_a's project; the baseline lives in org_b's.
	if err := store.Save(ctx, &run.Run{ID: "run_mine", ProjectID: "proj_solo",
		Status: run.StatusFailed, CreatedAt: at(10)}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.Save(ctx, &run.Run{ID: "run_theirs", ProjectID: "proj_b",
		Status: run.StatusSucceeded, CreatedAt: at(0)}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	handler := runCompareHandler(store, authz, zap.NewNop())

	// A member of org_a reads their run but must not read org_b's run through the comparison.
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/run_mine/compare?with=run_theirs", nil)
	req.SetPathValue("id", "run_mine")
	req = req.WithContext(ctxActor("user_member_a", user.RoleViewer))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want the baseline refused, not quoted", rec.Code)
	}
	if rec.Code == http.StatusOK {
		t.Fatalf("comparison quoted a run the caller cannot read: %s", rec.Body.String())
	}
}
