package importer

import (
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/schedule"
)

// TestImportedInventoryCannotWriteItsOwnDirectives checks that a name or variable out of somebody
// else's export cannot add lines to the inventory it is written into.
//
// A line break is the whole attack. A host named "web1\n[all:vars]\nansible_python_interpreter=..."
// does not land as a strange host name: it closes the line and opens a new section. That variable
// points Ansible at an arbitrary interpreter on the executor for every play in the run, and
// ansible_connection=local redirects the whole play onto the executor itself. Neither has a
// command-line counterpart, so an inventory setting them wins, and the content is written to a temp
// file and passed to ansible-playbook as -i verbatim.
func TestImportedInventoryCannotWriteItsOwnDirectives(t *testing.T) {
	t.Parallel()
	const interpreter = "ansible_python_interpreter"
	tests := []struct {
		Name  string
		Hosts []importHost
		Group []importGroup
	}{{ // Test 0: The host name carries the payload.
		Name: "host name",
		Hosts: []importHost{{
			Name: "web1\n[all:vars]\n" + interpreter + "=/tmp/evil\nansible_connection=local",
		}},
	}, { // Test 1: A host variable value carries it.
		Name: "variable value",
		Hosts: []importHost{{
			Name:      "web1",
			Variables: map[string]any{"ansible_user": "bob\n[all:vars]\n" + interpreter + "=/tmp/evil"},
		}},
	}, { // Test 2: A variable name carries it.
		Name: "variable name",
		Hosts: []importHost{{
			Name:      "web1",
			Variables: map[string]any{"x\n[all:vars]\n" + interpreter: "/tmp/evil"},
		}},
	}, { // Test 3: A group name carries it.
		Name:  "group name",
		Group: []importGroup{{Name: "db\n[all:vars]\n" + interpreter + "=/tmp/evil"}},
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			plan := &Plan{}
			got := buildInventoryINI(plan, "prod fleet", test.Hosts, test.Group)
			if strings.Contains(got, interpreter) {
				t.Errorf("the generated inventory sets %s, which points every play at an "+
					"interpreter of the export's choosing on the executor:\n%s", interpreter, got)
			}
			if strings.Contains(got, "ansible_connection=local") {
				t.Errorf("the generated inventory redirects the play onto the executor:\n%s", got)
			}
			// Whatever was dropped is reported, so a person reviewing the plan learns of it.
			if len(plan.Warnings) == 0 {
				t.Error("something was dropped from the inventory with no warning")
			}
			for _, w := range plan.Warnings {
				if strings.Contains(w, "\n") {
					t.Errorf("a warning spans lines, so the report can be dressed up as several "+
						"messages by the thing it reports on: %q", w)
				}
			}
		})
	}
}

// TestOrdinaryInventoryStillImports checks the guard did not break the normal case.
func TestOrdinaryInventoryStillImports(t *testing.T) {
	t.Parallel()
	plan := &Plan{}
	got := buildInventoryINI(plan, "prod", []importHost{
		{Name: "web1", Variables: map[string]any{"ansible_user": "deploy", "port": 22}},
		{Name: "web2"},
	}, []importGroup{{Name: "db", Hosts: []importHost{{Name: "db1"}}}})

	for _, want := range []string{"web1 ansible_user=deploy port=22", "web2", "[db]", "db1"} {
		if !strings.Contains(got, want) {
			t.Errorf("inventory is missing %q:\n%s", want, got)
		}
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("an ordinary inventory produced warnings: %v", plan.Warnings)
	}
}

// TestWarningsAreCapped checks that a hostile export cannot make one preview cost an unbounded
// response. Two warnings are emitted per unmapped credential and the document is somebody else's.
func TestWarningsAreCapped(t *testing.T) {
	t.Parallel()
	plan := &Plan{}
	for i := 0; i < maxWarnings*3; i++ {
		plan.warn("warning %d", i)
	}
	if len(plan.Warnings) > maxWarnings+1 {
		t.Errorf("plan holds %d warnings, want the cap to bound it", len(plan.Warnings))
	}
	if plan.Suppressed() == 0 {
		t.Error("warnings were dropped with no count, so a truncated report looks complete")
	}
}

// TestImportedSchedulesAreUsable checks that a schedule out of an export is validated and comes due.
//
// An unvalidated cron string became a stored row the scheduler logged an error over on every tick,
// and a nil first-fire time reads as "not due yet", so an imported schedule was reported as created
// and then never ran. A migrated nightly job that silently does not fire is the worst shape of this:
// nothing looks broken until the work has not happened for a month.
func TestImportedSchedulesAreUsable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	plan := &Plan{}
	plan.addSchedule(&schedule.Schedule{ID: "sch_1", Name: "nightly", Cron: "0 2 * * *", TemplateID: "tpl_1"}, "test", now)
	plan.addSchedule(&schedule.Schedule{ID: "sch_2", Name: "broken", Cron: "*/0 * * * *", TemplateID: "tpl_1"}, "test", now)
	plan.addSchedule(&schedule.Schedule{ID: "sch_3", Name: "nonsense", Cron: "not a cron at all ; rm -rf /", TemplateID: "tpl_1"}, "test", now)

	if len(plan.Schedules) != 1 {
		t.Fatalf("plan holds %d schedules, want only the valid one", len(plan.Schedules))
	}
	got := plan.Schedules[0]
	if got.Name != "nightly" {
		t.Errorf("kept %q, want the valid schedule", got.Name)
	}
	if got.NextRunAt == nil {
		t.Error("the schedule has no first fire time, so the scheduler skips it and it never runs")
	}
	if len(plan.Warnings) != 2 {
		t.Errorf("warnings = %v, want one for each schedule that was refused", plan.Warnings)
	}
}

// TestRRULECannotChooseTheCadence pins that an export cannot decide how often a schedule fires.
//
// The rule's fields were written into a cron expression unvalidated, so a schedule named "Nightly at
// 2am" carrying BYMINUTE=* and BYHOUR=* imported as "* * * * *": a run every minute, enabled, with
// no warning. Combined with a scheduler that reads an impossible expression as always due, an
// import file was a run storm.
func TestRRULECannotChooseTheCadence(t *testing.T) {
	t.Parallel()
	hostile := []string{
		"DTSTART:20260101T020000Z RRULE:FREQ=DAILY;BYMINUTE=*;BYHOUR=*",
		"DTSTART:20260101T020000Z RRULE:FREQ=DAILY;BYMINUTE=0,15,30,45",
		"DTSTART:20260101T020000Z RRULE:FREQ=DAILY;BYHOUR=*/1",
		"DTSTART:20260101T020000Z RRULE:FREQ=MONTHLY;BYMONTHDAY=*",
		"DTSTART:20260101T020000Z RRULE:FREQ=DAILY;BYMINUTE=99",
		"DTSTART:20260101T020000Z RRULE:FREQ=DAILY;BYHOUR=-1",
	}
	for _, rule := range hostile {
		got, ok := RRULEToCron(rule)
		if ok {
			t.Errorf("RRULEToCron(%q) produced %q; the file being imported chose the cadence",
				rule, got)
		}
	}

	// A step that does not divide its range evenly is not the cadence it looks like: "*/45" fires
	// at 0 and 45 past the hour, a 45 minute gap then a 15 minute one.
	uneven := []string{
		"DTSTART:20260101T000000Z RRULE:FREQ=MINUTELY;INTERVAL=45",
		"DTSTART:20260101T000000Z RRULE:FREQ=MINUTELY;INTERVAL=7",
		"DTSTART:20260101T000000Z RRULE:FREQ=HOURLY;INTERVAL=5",
		"DTSTART:20260101T000000Z RRULE:FREQ=HOURLY;INTERVAL=7",
	}
	for _, rule := range uneven {
		if got, ok := RRULEToCron(rule); ok {
			t.Errorf("RRULEToCron(%q) produced %q, which does not fire at that interval", rule, got)
		}
	}

	// Ordinary rules still convert, so the guard did not close the importer.
	fine := map[string]string{
		"DTSTART:20260101T020000Z RRULE:FREQ=DAILY":                      "0 2 * * *",
		"DTSTART:20260101T023000Z RRULE:FREQ=DAILY;BYMINUTE=30;BYHOUR=2": "30 2 * * *",
		"DTSTART:20260101T000000Z RRULE:FREQ=MINUTELY;INTERVAL=15":       "*/15 * * * *",
		"DTSTART:20260101T000000Z RRULE:FREQ=HOURLY;INTERVAL=6":          "0 */6 * * *",
	}
	for rule, want := range fine {
		got, ok := RRULEToCron(rule)
		if !ok {
			t.Errorf("RRULEToCron(%q) refused an ordinary rule", rule)
			continue
		}
		if got != want {
			t.Errorf("RRULEToCron(%q) = %q, want %q", rule, got, want)
		}
	}
}
