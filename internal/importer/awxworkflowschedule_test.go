package importer

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/schedule"
)

// scheduledWorkflowExport builds an export of one workflow whose schedules are spliced in at the
// place the caller names, so each shape a real export uses is exercised by the same document.
func scheduledWorkflowExport(schedules string) string {
	return `{
	  "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://e.com/i.git"}],
	  "job_templates": [{"name": "a", "playbook": "a.yml", "project": "infra"}],
	  "workflow_job_templates": [{
	    "name": "rollout",` + schedules + `
	    "workflow_nodes": [{"id": 1, "identifier": "first", "unified_job_template": "a"}]
	  }]
	}`
}

// nightlyRRule is the rrule of a workflow that fires at 2:30 every morning, the cadence a migrated
// workflow most often carries.
const nightlyRRule = `"rrule": "DTSTART:20260101T090000Z\n` +
	`RRULE:FREQ=DAILY;INTERVAL=1;BYHOUR=2;BYMINUTE=30"`

// TestWorkflowSchedulesImport is the data-loss case. A workflow job template's own schedules are
// what make it fire, and AWX carries them in two places depending on how the export was taken.
//
// Neither was read, so a workflow that fired nightly in AWX imported as a workflow template that
// never fires, and nothing in the report said so. The operator sees the workflow, its steps, and
// its graph, all correct, and finds out it is not running when the work it did stops happening.
func TestWorkflowSchedulesImport(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		Schedules string
		WantName  string
		WantCron  string
		WantZone  string
	}{{ // Test 0: The top-level shape, the same place an export can carry the graph's nodes.
		Name:      "top level",
		Schedules: `"schedules": [{"name": "nightly", ` + nightlyRRule + `}],`,
		WantName:  "nightly", WantCron: "30 2 * * *", WantZone: "UTC",
	}, { // Test 1: The nested shape, which awxkit writes.
		Name:      "nested under related",
		Schedules: `"related": {"schedules": [{"name": "nightly", ` + nightlyRRule + `}]},`,
		WantName:  "nightly", WantCron: "30 2 * * *", WantZone: "UTC",
	}, { // Test 2: The zone the rule names is carried, so 2am stays 2am where the operator lives.
		Name: "zone carried",
		Schedules: `"related": {"schedules": [{"name": "nightly", "rrule": ` +
			`"DTSTART;TZID=America/Chicago:20260101T023000\nRRULE:FREQ=DAILY;INTERVAL=1"}]},`,
		WantName: "nightly", WantCron: "30 2 * * *", WantZone: "America/Chicago",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			plan, err := FromAWX([]byte(scheduledWorkflowExport(test.Schedules)), importNow)
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			tpl := workflowTemplate(t, plan)
			if len(plan.Schedules) != 1 {
				t.Fatalf("schedules = %d, want 1: the workflow's own schedules were dropped.\n"+
					"warnings: %s", len(plan.Schedules), strings.Join(plan.Warnings, "\n"))
			}
			got := plan.Schedules[0]
			want := &schedule.Schedule{
				ID: got.ID, Name: test.WantName, Cron: test.WantCron, Timezone: test.WantZone,
				TemplateID: tpl.ID, Enabled: true, CreatedAt: importNow, NextRunAt: got.NextRunAt,
			}
			if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("schedule mismatch (-want +got):\n%s", diff)
			}
			if got.NextRunAt == nil {
				t.Error("the imported schedule has no first fire time, so the scheduler skips it forever")
			}
			if got.TemplateID != tpl.ID {
				t.Errorf("schedule fires template %q, want the workflow template %q",
					got.TemplateID, tpl.ID)
			}
		})
	}
}

// TestWorkflowScheduleCronCannotExpressIsReported pins that a workflow schedule cron cannot express
// is refused and named, the same way a job template's is, rather than dropped in silence.
func TestWorkflowScheduleCronCannotExpressIsReported(t *testing.T) {
	t.Parallel()
	doc := scheduledWorkflowExport(`"related": {"schedules": [{"name": "quarterly",
		"rrule": "DTSTART:20260101T020000Z RRULE:FREQ=YEARLY"}]},`)
	plan, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	if len(plan.Schedules) != 0 {
		t.Fatalf("schedules = %d, want 0: a rule cron cannot express must not become one",
			len(plan.Schedules))
	}
	if _, ok := warningContaining(t, plan.Warnings, `schedule "quarterly"`, `workflow "rollout"`,
		"cadence cannot be expressed as cron"); !ok {
		t.Errorf("the refused workflow schedule was not reported.\nwarnings: %v", plan.Warnings)
	}
}

// TestRefusedWorkflowReportsItsSchedules pins the worst shape of the same loss. A workflow this
// importer refuses has no template for a schedule to fire, so its schedules cannot come across
// either. Saying only that the workflow was refused understates it: the cadence goes with it.
func TestRefusedWorkflowReportsItsSchedules(t *testing.T) {
	t.Parallel()
	doc := `{
	  "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://e.com/i.git"}],
	  "job_templates": [{"name": "a", "playbook": "a.yml", "project": "infra"},
	                    {"name": "b", "playbook": "b.yml", "project": "infra"}],
	  "workflow_job_templates": [{
	    "name": "rollout",
	    "related": {"schedules": [{"name": "nightly", ` + nightlyRRule + `}]},
	    "workflow_nodes": [
	      {"id": 1, "identifier": "first", "unified_job_template": "a", "failure_nodes": [2]},
	      {"id": 2, "identifier": "second", "unified_job_template": "b"}
	    ]
	  }]
	}`
	plan, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	if len(plan.Schedules) != 0 {
		t.Fatalf("schedules = %d, want 0: a refused workflow has no template to fire",
			len(plan.Schedules))
	}
	if _, ok := warningContaining(t, plan.Warnings, `workflow "rollout"`, "1 schedule"); !ok {
		t.Errorf("the refused workflow's schedules were not reported.\nwarnings: %v", plan.Warnings)
	}
}

// TestUnnamedWorkflowReportsItsSchedulesWithoutAnEmptyName pins the report a workflow carrying no
// name produces. It is refused for the missing name, and saying its schedules went with workflow ""
// names nothing the operator can search the export for, so the sentence says what it is instead.
func TestUnnamedWorkflowReportsItsSchedulesWithoutAnEmptyName(t *testing.T) {
	t.Parallel()
	doc := `{
	  "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://e.com/i.git"}],
	  "job_templates": [{"name": "a", "playbook": "a.yml", "project": "infra"}],
	  "workflow_job_templates": [{
	    "name": "",
	    "related": {"schedules": [{"name": "nightly", ` + nightlyRRule + `}]},
	    "workflow_nodes": [{"id": 1, "identifier": "first", "unified_job_template": "a"}]
	  }]
	}`
	plan, err := FromAWX([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	if len(plan.Schedules) != 0 {
		t.Fatalf("schedules = %d, want 0: a refused workflow has no template to fire",
			len(plan.Schedules))
	}
	got, ok := warningContaining(t, plan.Warnings,
		"the workflow job template without a name", "1 schedule")
	if !ok {
		t.Fatalf("the unnamed workflow's schedules were not reported.\nwarnings: %v", plan.Warnings)
	}
	if strings.Contains(got, `workflow ""`) {
		t.Errorf("the report names an empty workflow: %q", got)
	}
}
