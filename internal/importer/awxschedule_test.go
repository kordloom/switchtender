package importer

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/template"
)

// TestAWXScheduleOverridesComeAcross pins what an AWX schedule carries beyond its cadence.
//
// A schedule there launches with its own survey answers and extra variables, and with any limit,
// tags, or check mode it was saved with. Every one was dropped, so a nightly check against the canary
// host imported as a real run against the template's whole inventory, with none of the answers the
// playbook expected. A schedule that overrides anything now fires a copy of its template with the
// overrides applied, and the template people launch by hand is left as it was.
func TestAWXScheduleOverridesComeAcross(t *testing.T) {
	t.Parallel()
	const export = `{
	  "projects": [{"name": "web", "scm_type": "git", "scm_url": "https://example.com/web.git"}],
	  "inventory": [{"name": "prod", "hosts": [{"name": "web1"}]}],
	  "job_templates": [{"name": "Deploy", "playbook": "site.yml", "project": "web",
	    "inventory": "prod", "extra_vars": "channel: stable\nrelease: '1.0'",
	    "related": {"schedules": [
	      {"name": "nightly canary", "rrule": "DTSTART:20260101T020000Z RRULE:FREQ=DAILY;INTERVAL=1",
	       "extra_data": {"release": "1.2"}, "limit": "canary", "job_type": "check"},
	      {"name": "plain", "rrule": "DTSTART:20260101T030000Z RRULE:FREQ=DAILY;INTERVAL=1",
	       "extra_data": {}, "limit": "", "job_type": null, "diff_mode": null},
	      {"name": "elsewhere", "rrule": "DTSTART:20260101T040000Z RRULE:FREQ=DAILY;INTERVAL=1",
	       "inventory": "missing"}
	    ]}}]}`
	plan, err := FromAWX([]byte(export), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	byName := map[string]*template.Template{}
	for _, tpl := range plan.Templates {
		byName[tpl.Name] = tpl
	}
	orig, canary := byName["Deploy"], byName["Deploy (nightly canary)"]
	if orig == nil || canary == nil {
		t.Fatalf("templates = %v, want the original and a copy for the schedule that overrides it",
			plan.Templates)
	}
	want := template.Template{
		Name: "Deploy (nightly canary)", Playbook: "site.yml", Limit: "canary", DryRun: true,
		ExtraVars: map[string]any{"channel": "stable", "release": "1.2"},
	}
	if diff := cmp.Diff(want, *canary, cmpopts.IgnoreFields(template.Template{}, "ID", "ProjectID",
		"InventoryID", "CreatedAt"), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the schedule's copy mismatch (-want +got):\n%s", diff)
	}
	if canary.ProjectID != orig.ProjectID || canary.InventoryID != orig.InventoryID {
		t.Errorf("the copy lost the template's project or inventory: %+v", canary)
	}
	if orig.Limit != "" || orig.DryRun || orig.ExtraVars["release"] != "1.0" {
		t.Errorf("the schedule's overrides reached the template people launch by hand: %+v", orig)
	}
	fires := map[string]string{}
	armed := map[string]bool{}
	for _, sc := range plan.Schedules {
		for _, tpl := range plan.Templates {
			if tpl.ID == sc.TemplateID {
				fires[sc.Name] = tpl.Name
			}
		}
		armed[sc.Name] = sc.Enabled
	}
	wantFires := map[string]string{
		"nightly canary": "Deploy (nightly canary)", "plain": "Deploy", "elsewhere": "Deploy (elsewhere)",
	}
	if diff := cmp.Diff(wantFires, fires); diff != "" {
		t.Errorf("which template each schedule fires mismatch (-want +got):\n%s", diff)
	}
	if armed["elsewhere"] {
		t.Error("a schedule whose inventory did not come across imported armed, so it would fire " +
			"against the template's own inventory instead of the one it named")
	}
	if !armed["nightly canary"] || !armed["plain"] {
		t.Errorf("schedules that came across whole were switched off: %v", armed)
	}
	if _, ok := warningContaining(t, plan.Warnings, `schedule "nightly canary"`, "limit canary",
		"job type check", `"Deploy (nightly canary)"`); !ok {
		t.Errorf("the copy and what it carries were not reported.\nwarnings: %v", plan.Warnings)
	}
}
