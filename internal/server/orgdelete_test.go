package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// TestDeletingAnOrgRefusesWhileItsWorkStillExists covers the offboarding case.
//
// A schedule stamps the organization it was created in, and nothing revalidates that when it fires.
// Deleting the organization therefore left its nightly playbooks launching with real credentials
// under a tenant that no longer existed, and under strict grants those runs then belong to nobody,
// so they are visible to admins alone. Naming what is still attached is the answer a credential and
// a project already give for the same situation.
func TestDeletingAnOrgRefusesWhileItsWorkStillExists(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	orgs := org.NewMemStore()
	schedules := schedule.NewMemStore()
	templates := template.NewMemStore()

	if err := orgs.Save(ctx, &org.Org{ID: "org_1", Name: "platform"}); err != nil {
		t.Fatalf("Save org: %v", err)
	}
	next := time.Now().Add(time.Hour)
	if err := schedules.Save(ctx, &schedule.Schedule{
		ID: "sch_1", Name: "nightly patch", Cron: "0 2 * * *", OrgID: "org_1",
		Enabled: true, NextRunAt: &next,
	}); err != nil {
		t.Fatalf("Save schedule: %v", err)
	}

	refs := &refChecker{schedules: schedules, templates: templates}
	handler := deleteOrgHandler(orgs, refs, zap.NewNop())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/v1/orgs/org_1", nil)
	req.SetPathValue("id", "org_1")
	handler(rec, req)

	if rec.Code != 409 {
		t.Fatalf("delete returned %d, want 409 while a schedule still belongs to the org", rec.Code)
	}
	var body struct {
		Error  string              `json:"error"`
		UsedBy map[string][]string `json:"used_by"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := body.UsedBy["schedules"]; len(got) != 1 || got[0] != "nightly patch" {
		t.Errorf("used_by schedules = %v, want the schedule named so it can be detached", got)
	}
	if _, err := orgs.Get(ctx, "org_1"); err != nil {
		t.Errorf("the org was deleted anyway: %v", err)
	}

	// Once nothing belongs to it, the delete goes through.
	if err := schedules.Delete(ctx, "sch_1"); err != nil {
		t.Fatalf("Delete schedule: %v", err)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("DELETE", "/v1/orgs/org_1", nil)
	req.SetPathValue("id", "org_1")
	handler(rec, req)
	if rec.Code != 200 {
		t.Fatalf("delete returned %d after detaching everything, want 200", rec.Code)
	}
}
