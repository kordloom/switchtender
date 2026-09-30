package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// TestDoctorFindingsNameSomethingYouCanOpen covers a report an operator cannot act on.
//
// A name is optional on a template, a schedule and a credential, and the API creates unnamed ones
// without complaint. The schedule tutorial's own copyable command produces one. The doctor then
// listed "schedule  | Fires template tpl_abc123, which no longer exists" with an empty space where
// the identity should be, twice over, and a reader working through a list of problems had no way to
// tell which object each one meant.
func TestDoctorFindingsNameSomethingYouCanOpen(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		In   string
		ID   string
		Want string
	}{{ // Test 0: A named object is called by its name.
		Name: "named", In: "nightly audit", ID: "sch_1", Want: "nightly audit",
	}, { // Test 1: An unnamed one falls back to the id, which is what the reader searches for.
		Name: "unnamed", In: "", ID: "sch_50acef79e54db9c5", Want: "sch_50acef79e54db9c5",
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			if got := namedOr(test.In, test.ID); got != test.Want {
				t.Errorf("%s: namedOr(%q, %q) = %q, want %q: a finding nobody can act on is not a "+
					"finding", test.Name, test.In, test.ID, got, test.Want)
			}
		})
	}
}

// TestDoctorDoesNotSayAMissingReferenceWasRemoved covers the wording of a reference that resolves
// to nothing. The doctor cannot tell an object that was deleted from an id that never existed, and
// the schedule tutorial's copyable command names tpl_abc123, which never did. Every such finding
// said the object "no longer exists", telling the reader something had been removed from under them
// when the id was only mistyped.
func TestDoctorDoesNotSayAMissingReferenceWasRemoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tpls := template.NewMemStore()
	if err := tpls.Save(ctx, &template.Template{
		ID: "tpl_1", Name: "deploy", Playbook: "site.yml",
		InventoryID: "inv_typo", ProjectID: "proj_typo", CredentialIDs: []string{"cred_typo"},
		SelectableCredentialIDs: []string{"cred_pick_typo"}, PullCredentialID: "cred_pull_typo",
	}); err != nil {
		t.Fatalf("Save(template) error = %v", err)
	}
	scheds := schedule.NewMemStore()
	if err := scheds.Save(ctx, &schedule.Schedule{
		ID: "sch_1", Name: "nightly", Cron: "0 2 * * *", TemplateID: "tpl_abc123",
	}); err != nil {
		t.Fatalf("Save(schedule) error = %v", err)
	}
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithTemplates(tpls), WithSchedules(scheds), WithInventories(inventory.NewMemStore()),
		WithProjects(project.NewMemStore()), WithCredentials(credential.NewMemStore(), nil)).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/doctor", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("doctor = %d, body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "no longer exists") {
		t.Errorf("a finding says an id that may never have existed no longer exists: %s", body)
	}
	for _, id := range []string{
		"inv_typo", "proj_typo", "cred_typo", "cred_pick_typo", "cred_pull_typo", "tpl_abc123",
	} {
		if !strings.Contains(body, id+", which does not exist.") {
			t.Errorf("no finding says %s does not exist: %s", id, body)
		}
	}
}
