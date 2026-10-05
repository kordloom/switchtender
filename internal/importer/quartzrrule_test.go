package importer

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/schedule"
)

// TestQuartzToRRule pins the Quartz forms cron cannot read, each converted to the recurrence that
// fires at the same moments, and the forms one rule cannot say exactly, which stay refused.
func TestQuartzToRRule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantRule  string
		WantFirst string
		Crontab   string
		WantOK    bool
	}{{ // Test 0: The third Friday.
		Crontab: "0 0 9 ? * 6#3", WantOK: true,
		WantRule:  "FREQ=MONTHLY;BYDAY=3FR;BYHOUR=9;BYMINUTE=0;BYSECOND=0",
		WantFirst: "2026-09-18T09:00:00Z",
	}, { // Test 1: The last Friday of each quarter.
		Crontab: "0 0 17 ? 3,6,9,12 6L", WantOK: true,
		WantRule:  "FREQ=MONTHLY;BYMONTH=3,6,9,12;BYDAY=-1FR;BYHOUR=17;BYMINUTE=0;BYSECOND=0",
		WantFirst: "2026-09-25T17:00:00Z",
	}, { // Test 2: The last day of the month.
		Crontab: "0 30 2 L * ?", WantOK: true,
		WantRule:  "FREQ=MONTHLY;BYMONTHDAY=-1;BYHOUR=2;BYMINUTE=30;BYSECOND=0",
		WantFirst: "2026-09-30T02:30:00Z",
	}, { // Test 3: Two days before the last day.
		Crontab: "0 30 2 L-2 * ?", WantOK: true,
		WantRule:  "FREQ=MONTHLY;BYMONTHDAY=-3;BYHOUR=2;BYMINUTE=30;BYSECOND=0",
		WantFirst: "2026-09-28T02:30:00Z",
	}, { // Test 4: The last weekday of the month, which BYSETPOS picks.
		Crontab: "0 0 18 LW * ?", WantOK: true,
		WantRule: "FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1;BYHOUR=18;BYMINUTE=0;" +
			"BYSECOND=0",
		WantFirst: "2026-09-30T18:00:00Z",
	}, { // Test 5: A stepped minute expands into a list.
		Crontab: "0 0/15 9 ? * MON#1", WantOK: true,
		WantRule:  "FREQ=MONTHLY;BYDAY=1MO;BYHOUR=9;BYMINUTE=0,15,30,45;BYSECOND=0",
		WantFirst: "2026-09-07T09:00:00Z",
	}, { // Test 6: A year limit starts and stops the rule, which cron had to drop.
		Crontab: "30 0 9 ? JAN-MAR 2#1 2027", WantOK: true,
		WantRule: "FREQ=MONTHLY;BYMONTH=1,2,3;BYDAY=1MO;BYHOUR=9;BYMINUTE=0;BYSECOND=30;" +
			"UNTIL=20271231T235959",
		WantFirst: "2027-01-04T09:00:30Z",
	}, { // Test 7: The last weekday at two times of day would keep only the later one.
		Crontab: "0 0 6,18 LW * ?",
	}, { // Test 8: Both day fields set is not a Quartz expression.
		Crontab: "0 0 9 L * 6L",
	}, { // Test 9: An nth weekday past the fifth.
		Crontab: "0 0 9 ? * 6#6",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Crontab), func(t *testing.T) {
			t.Parallel()
			fields := strings.Fields(test.Crontab)
			if len(fields) == 6 {
				fields = append(fields, "*")
			}
			if !quartzNeedsRecurrence(fields) {
				t.Fatalf("quartzNeedsRecurrence(%q) = false, want true", test.Crontab)
			}
			got, ok := quartzToRRule(fields, importNow)
			if ok != test.WantOK {
				t.Fatalf("quartzToRRule(%q) ok = %v, want %v (rule %q)", test.Crontab, ok,
					test.WantOK, got)
			}
			if !ok {
				return
			}
			_, rule, _ := strings.Cut(got, "RRULE:")
			if diff := cmp.Diff(test.WantRule, rule); diff != "" {
				t.Errorf("rule mismatch (-want +got):\n%s", diff)
			}
			sc := &schedule.Schedule{RRule: got, Timezone: "UTC", Playbook: "p"}
			first, err := sc.NextFire(importNow)
			if err != nil {
				t.Fatalf("NextFire() error = %v", err)
			}
			if diff := cmp.Diff(test.WantFirst, first.UTC().Format(time.RFC3339)); diff != "" {
				t.Errorf("first fire mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRundeckNthWeekdayJobComesAcross pins the end to end path: a Rundeck job on the third Friday
// used to be refused because cron has no reading for it, and now imports as a recurrence.
func TestRundeckNthWeekdayJobComesAcross(t *testing.T) {
	t.Parallel()
	doc := "- name: settle\n  schedule:\n    crontab: '0 0 9 ? * 6#3'\n" +
		"  sequence:\n    commands:\n      - exec: /bin/settle\n"
	plan := rundeckPlan(t, "prod", doc)
	if len(plan.Schedules) != 1 {
		t.Fatalf("schedules = %d, want 1.\nwarnings: %v", len(plan.Schedules), plan.Warnings)
	}
	if got := plan.Schedules[0]; got.Cron != "" || !strings.Contains(got.RRule, "BYDAY=3FR") {
		t.Errorf("schedule cron = %q rrule = %q, want the third Friday as a rule", got.Cron,
			got.RRule)
	}
	if w, ok := warningContaining(t, plan.Warnings, "no cron equivalent"); ok {
		t.Errorf("the job came across and is still reported as refused: %s", w)
	}
}
