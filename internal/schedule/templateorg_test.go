package schedule

import (
	"context"
	"fmt"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/template"
)

// TestTemplateFireFallsBackToTheTemplatesOrg covers the owner a template-fired run is stamped with
// when the schedule itself carries no organization.
//
// A schedule created outside an actor's request, such as a crontab import or a seeded demo, has an
// empty OrgID. Firing one that names a stored template must fall back to the template's own
// organization, because a run stamped with nothing is ownerless, and under strict grants an
// ownerless run is denied to every non-admin. The tenant would see the template and the schedule and
// none of the runs the pair produced.
//
// The existing org coverage sets both organizations to the same value, so the fallback branch is
// never taken and deleting it changes no assertion. This pins each combination separately.
func TestTemplateFireFallsBackToTheTemplatesOrg(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		// ScheduleOrg is the organization stamped on the schedule, empty for one created outside a
		// request.
		ScheduleOrg string
		// TemplateOrg is the organization owning the stored template.
		TemplateOrg string
		// WantOrg is the organization the fired run must carry.
		WantOrg string
	}{{ // Test 0: The schedule's own organization wins when it has one.
		ScheduleOrg: "org_acme", TemplateOrg: "org_acme", WantOrg: "org_acme",
	}, { // Test 1: An imported schedule carries none, so the template's organization stands in.
		ScheduleOrg: "", TemplateOrg: "org_acme", WantOrg: "org_acme",
	}, { // Test 2: The schedule's organization wins even where the template names a different one,
		// since the schedule is the thing the tenant created and can see.
		ScheduleOrg: "org_acme", TemplateOrg: "org_other", WantOrg: "org_acme",
	}, { // Test 3: Neither carries one, so there is nothing to stamp and the run stays ownerless.
		ScheduleOrg: "", TemplateOrg: "", WantOrg: "",
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			templates := template.NewMemStore()
			tpl := &template.Template{
				ID: "tpl_1", Name: "deploy", Playbook: "site.yml", Inventory: "prod",
				OrgID: test.TemplateOrg,
			}
			if err := templates.Save(ctx, tpl); err != nil {
				t.Fatalf("Save() template error = %v", err)
			}
			sc := &Schedule{
				ID: "sch_tpl", Name: "nightly template", Cron: "0 3 * * *", TemplateID: "tpl_1",
				Enabled: true, OrgID: test.ScheduleOrg,
			}
			schedules := NewMemStore()
			if err := schedules.Save(ctx, sc); err != nil {
				t.Fatalf("Save() schedule error = %v", err)
			}
			rec := &recordingSubmitter{}
			s := NewScheduler(schedules, rec, zap.NewNop(), WithTemplates(templates))

			if _, err := s.fire(ctx, sc); err != nil {
				t.Fatalf("fire() error = %v", err)
			}
			if rec.got == nil {
				t.Fatal("fire() reached no submitter")
			}
			if rec.got.OrgID != test.WantOrg {
				t.Errorf("a template fired by a schedule with org %q and template org %q produced a "+
					"run owned by %q, want %q: an ownerless run is denied to every non-admin under "+
					"strict grants", test.ScheduleOrg, test.TemplateOrg, rec.got.OrgID, test.WantOrg)
			}
		})
	}
}
