package importer

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/schedule"
)

// TestAWXRuleCarriedAsCronKeepsAWXSpringForward pins that an AWX rule imported as a cron expression
// fires where AWX would on the night the clocks go forward over it.
//
// AWX reads a time the clocks skip as the length of the jump later, and a rule kept as a rule does
// the same. A rule that a cron expression says exactly is carried as cron, and a cron expression
// jumps by default, so a nightly 02:30 job imported in October fired at 03:00 the next March where
// AWX fired it at 03:30. It also pins that the import does not depend on when it ran: a rule imported within a
// dozen days of that night comes across as the same cron expression as one imported in October.
func TestAWXRuleCarriedAsCronKeepsAWXSpringForward(t *testing.T) {
	t.Parallel()
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation() error = %v", err)
	}
	const export = `{
	  "projects": [{"name": "web", "scm_type": "git", "scm_url": "https://example.com/web.git"}],
	  "job_templates": [{"name": "Nightly", "playbook": "site.yml", "project": "web",
	    "related": {"schedules": [{"name": "nightly",
	      "rrule":
	        "DTSTART;TZID=America/New_York:20260101T023000\nRRULE:FREQ=DAILY;INTERVAL=1"}]}}]}`
	tests := []struct {
		Now        time.Time
		WantCron   string
		WantFireAt string
	}{{ // Test 0: Imported in October, months from the night.
		Now: time.Date(2026, 10, 1, 12, 0, 0, 0, ny), WantCron: "30 2 * * *",
		WantFireAt: "2027-03-14 03:30 EDT",
	}, { // Test 1: Imported a week before the night, the same cron and the same fire.
		Now: time.Date(2027, 3, 7, 12, 0, 0, 0, ny), WantCron: "30 2 * * *",
		WantFireAt: "2027-03-14 03:30 EDT",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			plan, err := FromAWX([]byte(export), test.Now)
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			if len(plan.Schedules) != 1 {
				t.Fatalf("schedules = %d, want 1", len(plan.Schedules))
			}
			sc := plan.Schedules[0]
			next, err := sc.NextFire(time.Date(2027, 3, 13, 12, 0, 0, 0, ny))
			if err != nil {
				t.Fatalf("NextFire() error = %v", err)
			}
			got := []string{sc.Cron, sc.SpringForwardSetting(),
				next.In(ny).Format("2006-01-02 15:04 MST")}
			want := []string{test.WantCron, schedule.SpringForwardLater, test.WantFireAt}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("imported schedule mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
