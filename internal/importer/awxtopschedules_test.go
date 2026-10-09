package importer

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// topScheduleRule is a rule every schedule in these tests fires on, so each test varies only what
// the schedule belongs to.
const topScheduleRule = "DTSTART:20260101T020000Z RRULE:FREQ=DAILY;INTERVAL=1"

// topSchedule writes one top-level schedule the way awxkit writes it, naming what it fires by
// natural key with its kind, or by bare name when kind is empty.
func topSchedule(name, kind, target string) string {
	ref := `"` + target + `"`
	if kind != "" {
		ref = `{"name": "` + target + `", "type": "` + kind + `"}`
	}
	return topScheduleRef(name, ref)
}

// topScheduleRef writes one top-level schedule whose unified_job_template is ref, written verbatim.
func topScheduleRef(name, ref string) string {
	return `{"name": "` + name + `", "rrule": "` + topScheduleRule + `", "unified_job_template": ` +
		ref + `, "description": ""}`
}

// TestAWXTopLevelSchedulesArePlacedOrNamed pins the top-level schedule list awxkit writes from
// AWX 22 on: each schedule there goes with its job template or workflow, is the same schedule as
// its nested copy, or is named in the report with the reason it did not import, and the list is
// never reported as a field this importer does not read.
func TestAWXTopLevelSchedulesArePlacedOrNamed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the subtest.
		Name string
		// Org is the organization the job template and the workflow belong to, or empty for none.
		Org string
		// Schedules is the export's top-level schedule list.
		Schedules string
		// WantSchedules names the schedules the plan holds, in order.
		WantSchedules []string
		// WantWarnings are fragments of warnings that must appear.
		WantWarnings []string
		// WantRefused is how many top-level schedules were named as not imported.
		WantRefused int
	}{{ // Test 0: The nested copy and the top-level copy are one schedule.
		Name:          "duplicate of a nested schedule",
		Schedules:     topSchedule("Nightly", "job_template", "Deploy"),
		WantSchedules: []string{"Nightly", "Monthly"},
	}, { // Test 1: A schedule only the top level carries imports with its job template.
		Name:          "job template schedule at the top level only",
		Schedules:     topSchedule("Weekly", "job_template", "Deploy"),
		WantSchedules: []string{"Nightly", "Weekly", "Monthly"},
	}, { // Test 2: The same for a workflow.
		Name:          "workflow schedule at the top level only",
		Schedules:     topSchedule("Quarterly", "workflow_job_template", "Release"),
		WantSchedules: []string{"Nightly", "Monthly", "Quarterly"},
	}, { // Test 3: A bare name declares no kind and is matched to the template of that name.
		Name:          "bare reference",
		Schedules:     topSchedule("Weekly", "", "Deploy"),
		WantSchedules: []string{"Nightly", "Weekly", "Monthly"},
	}, { // Test 4: A project update and a system job have nothing here to fire, and are named once.
		Name: "project update and system job",
		Schedules: topSchedule("Sync infra", "project", "infra") + "," +
			topSchedule("Cleanup Expired Sessions", "system_job_template", "Cleanup Expired Sessions"),
		WantSchedules: []string{"Nightly", "Monthly"},
		WantWarnings: []string{`2 schedules at the top level of the export fire a project update, an ` +
			`inventory sync, or a system job, which nothing here stands in for, so they were not ` +
			`imported: "Sync infra", "Cleanup Expired Sessions"`},
		WantRefused: 2,
	}, { // Test 5: A schedule of a job template the export does not hold is named beside it.
		Name:          "owner not in the export",
		Schedules:     topSchedule("Lost", "job_template", "Missing"),
		WantSchedules: []string{"Nightly", "Monthly"},
		WantWarnings: []string{`schedule "Lost" fires job template "Missing", which did not import, ` +
			"so it was not imported either"},
		WantRefused: 1,
	}, { // Test 6: A bare name finds the one template of that name inside its organization.
		Name:          "bare reference to a template in an organization",
		Org:           "Default",
		Schedules:     topSchedule("Weekly", "", "Deploy"),
		WantSchedules: []string{"Nightly", "Weekly", "Monthly"},
	}, { // Test 7: The same for a workflow in an organization.
		Name:          "bare reference to a workflow in an organization",
		Org:           "Default",
		Schedules:     topSchedule("Quarterly", "", "Release"),
		WantSchedules: []string{"Nightly", "Monthly", "Quarterly"},
	}, { // Test 8: A natural key that names the organization is placed inside it.
		Name: "natural key with an organization",
		Org:  "Default",
		Schedules: topScheduleRef("Weekly", `{"name": "Deploy", "type": "job_template", `+
			`"organization": {"name": "Default", "type": "organization"}}`),
		WantSchedules: []string{"Nightly", "Weekly", "Monthly"},
	}, { // Test 9: An id, the shape the REST API writes, is named with why it was not placed.
		Name:          "numeric id",
		Org:           "Default",
		Schedules:     topScheduleRef("ById", "7"),
		WantSchedules: []string{"Nightly", "Monthly"},
		WantWarnings: []string{`schedule "ById" fires AWX id 7, and a top-level schedule is placed ` +
			"with its template by name, not by id, so it was not imported"},
		WantRefused: 1,
	}, { // Test 10: A bare name the export does not hold is not called a project update.
		Name:          "bare reference to nothing in the export",
		Org:           "Default",
		Schedules:     topSchedule("Stray", "", "Elsewhere"),
		WantSchedules: []string{"Nightly", "Monthly"},
		WantWarnings: []string{`schedule "Stray" fires "Elsewhere", which this export does not hold, ` +
			"so it was not imported"},
		WantRefused: 1,
	}, { // Test 11: A bare name a job template and a workflow share is not guessed at.
		Name:          "bare reference both a template and a workflow answer to",
		Schedules:     topSchedule("Shared", "", "Twin"),
		WantSchedules: []string{"Nightly", "Monthly"},
		WantWarnings: []string{`schedule "Shared" fires "Twin", a name more than one job template ` +
			"or workflow in this export answers to, and the reference does not say which, so it " +
			"was not imported"},
		WantRefused: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			org := `""`
			if test.Org != "" {
				org = `{"name": "` + test.Org + `", "type": "organization"}`
			}
			export := `{
			  "organizations": [{"name": "Default"}],
			  "projects": [{"name": "infra", "scm_type": "git", "scm_url": "https://e.com/i.git",
			    "organization": ` + org + `}],
			  "job_templates": [{"name": "Deploy", "playbook": "site.yml", "project": "infra",
			    "organization": ` + org + `,
			    "related": {"schedules": [` + topSchedule("Nightly", "job_template", "Deploy") + `]}},
			    {"name": "Twin", "playbook": "site.yml", "project": "infra", "organization": ` + org + `}],
			  "workflow_job_templates": [{"name": "Release", "organization": ` + org + `, "related": {
			    "workflow_nodes": [{"identifier": "deploy", "unified_job_template": {"name": "Deploy"}}],
			    "schedules": [` + topSchedule("Monthly", "workflow_job_template", "Release") + `]}},
			    {"name": "Twin", "organization": ` + org + `, "related": {"workflow_nodes": [
			      {"identifier": "deploy", "unified_job_template": {"name": "Deploy"}}]}}],
			  "schedules": [` + test.Schedules + `]
			}`
			plan, err := FromAWX([]byte(export), importNow)
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			var got []string
			for _, s := range plan.Schedules {
				got = append(got, s.Name)
			}
			if diff := cmp.Diff(test.WantSchedules, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("schedules mismatch (-want +got):\n%s\nwarnings: %v", diff, plan.Warnings)
			}
			for _, want := range test.WantWarnings {
				if _, ok := warningContaining(t, plan.Warnings, want); !ok {
					t.Errorf("no warning says %q.\nwarnings: %v", want, plan.Warnings)
				}
			}
			if plan.refused != test.WantRefused {
				t.Errorf("refused = %d, want %d", plan.refused, test.WantRefused)
			}
			// The whole point: the word schedules never appears among the fields this importer does
			// not read, and neither does anything a schedule carries.
			for _, w := range plan.Warnings {
				if strings.Contains(w, "does not read") && strings.Contains(w, "schedules") {
					t.Errorf("schedules are still reported as unread: %s", w)
				}
			}
			report := plan.Report()
			for _, want := range test.WantWarnings {
				if !slices.ContainsFunc(report.LeftOut, func(w string) bool {
					return strings.Contains(w, want)
				}) {
					t.Errorf("the schedule that did not import is not itemized as left out: %v",
						report.LeftOut)
				}
			}
		})
	}
}
