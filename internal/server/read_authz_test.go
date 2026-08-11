package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/user"
)

// TestReadsAreScopedToGrants proves the change-automation map is not readable across a grant
// boundary.
//
// Every mutating handler for schedules, triggers, and inventory sources authorized the object behind
// it, and every reading handler authorized nothing. Fetching a schedule by id was therefore a direct
// object reference, and listing returned the whole estate: which templates fire unattended, on what
// cadence, against which inventory, which credential each dynamic source borrows, and each source's
// last error, which is verbatim plugin output.
func TestReadsAreScopedToGrants(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	schedules := schedule.NewMemStore()
	mineAt := time.Date(2026, 8, 10, 2, 0, 0, 0, time.UTC)
	for _, sc := range []*schedule.Schedule{
		{ID: "sch_mine", Name: "mine", Cron: "0 2 * * *", TemplateID: "tpl_mine",
			Enabled: true, CreatedAt: mineAt, NextRunAt: &mineAt},
		{ID: "sch_theirs", Name: "their-secret-window", Cron: "0 3 * * *", TemplateID: "tpl_theirs",
			Enabled: true, CreatedAt: mineAt, NextRunAt: &mineAt},
	} {
		if err := schedules.Save(ctx, sc); err != nil {
			t.Fatalf("Save(%s) error = %v", sc.ID, err)
		}
	}

	triggers := trigger.NewMemStore()
	for _, tg := range []*trigger.Trigger{
		{ID: "trg_mine", Name: "mine", TemplateID: "tpl_mine", TokenHash: "hash_a", CreatedAt: mineAt},
		{ID: "trg_theirs", Name: "their-secret-hook", TemplateID: "tpl_theirs", TokenHash: "hash_b",
			CreatedAt: mineAt},
	} {
		if err := triggers.Save(ctx, tg); err != nil {
			t.Fatalf("Save(%s) error = %v", tg.ID, err)
		}
	}

	sources := invsource.NewMemStore()
	for _, src := range []*invsource.Source{
		{ID: "src_mine", Name: "mine", ProjectID: "proj_mine", InventoryID: "inv_mine",
			CreatedAt: mineAt},
		{ID: "src_theirs", Name: "their-secret-source", ProjectID: "proj_theirs",
			InventoryID: "inv_theirs", CredentialID: "cred_theirs", CreatedAt: mineAt},
	} {
		if err := sources.Save(ctx, src); err != nil {
			t.Fatalf("Save(%s) error = %v", src.ID, err)
		}
	}

	// Strict grants: the caller may use only their own objects.
	grants := &fakeGrants{byObject: map[string][]*grant.Grant{
		"tpl_mine":  {{Subject: "user_1", Access: grant.AccessUse}},
		"proj_mine": {{Subject: "user_1", Access: grant.AccessUse}},
		"inv_mine":  {{Subject: "user_1", Access: grant.AccessUse}},
	}}
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithSchedules(schedules), WithTriggers(triggers, nil), WithInventorySources(sources, nil),
		WithGrants(grants, true)).Handler()

	as := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(context.WithValue(req.Context(), actorKey{},
			Actor{UserID: "user_1", Role: user.RoleOperator}))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	for _, tc := range []struct{ path, leaked, kept string }{
		{"/v1/schedules", "their-secret-window", "sch_mine"},
		{"/v1/triggers", "their-secret-hook", "trg_mine"},
		{"/v1/inventory-sources", "their-secret-source", "src_mine"},
	} {
		rec := as(tc.path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200 (%s)", tc.path, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), tc.leaked) {
			t.Errorf("GET %s returned %q, which belongs to another grant boundary:\n%s",
				tc.path, tc.leaked, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), tc.kept) {
			t.Errorf("GET %s dropped the caller's own %q, so the filter is too strict:\n%s",
				tc.path, tc.kept, rec.Body.String())
		}
	}

	// Fetching another boundary's schedule by id must not answer with it.
	rec := as("/v1/schedules/sch_theirs")
	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "their-secret-window") {
		t.Errorf("GET /v1/schedules/sch_theirs answered with another boundary's schedule: %s",
			rec.Body.String())
	}
	// The caller's own is still reachable by id.
	rec = as("/v1/schedules/sch_mine")
	if rec.Code != http.StatusOK {
		t.Errorf("GET /v1/schedules/sch_mine = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var got schedule.Schedule
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err == nil && got.ID != "sch_mine" {
		t.Errorf("fetched %q, want sch_mine", got.ID)
	}
}
