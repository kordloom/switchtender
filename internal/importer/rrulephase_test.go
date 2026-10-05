package importer

import (
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/schedule"
)

// TestAStepKeepsTheOffsetTheRuleStartsAt covers a cadence that came across shifted.
//
// An interval was written into cron as a bare step, which loses the phase. Every 15 minutes from :07 is
// 07, 22, 37, 52; "*/15" is 00, 15, 30, 45. For an hourly rule the same loss moves a maintenance window
// by hours: every 6 hours from 02:00 is 02, 08, 14, 20, and "*/6" is 00, 06, 12, 18. The expression
// looked right, the schedule validated, and the window was somewhere else.
func TestAStepKeepsTheOffsetTheRuleStartsAt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case is.
		Name string
		// RRule is the AWX recurrence field.
		RRule string
		// WantCron is the expression the rule means.
		WantCron string
		// WantFires are the first times the imported schedule fires, to prove the expression is not
		// merely a different string but the same instants the rule names.
		WantFires []string
	}{{
		Name:      "every 15 minutes from seven past",
		RRule:     "DTSTART:20260101T000700Z RRULE:FREQ=MINUTELY;INTERVAL=15",
		WantCron:  "7-59/15 * * * *",
		WantFires: []string{"2026-01-01 00:07", "2026-01-01 00:22", "2026-01-01 00:37"},
	}, {
		Name:      "every six hours from two in the morning",
		RRule:     "DTSTART:20260101T020000Z RRULE:FREQ=HOURLY;INTERVAL=6",
		WantCron:  "0 2-23/6 * * *",
		WantFires: []string{"2026-01-01 02:00", "2026-01-01 08:00", "2026-01-01 14:00"},
	}, {
		Name:      "every four hours from midnight is still a plain step",
		RRule:     "DTSTART:20260101T000000Z RRULE:FREQ=HOURLY;INTERVAL=4",
		WantCron:  "0 */4 * * *",
		WantFires: []string{"2026-01-01 04:00", "2026-01-01 08:00", "2026-01-01 12:00"},
	}, {
		Name:      "every minute needs no offset",
		RRule:     "DTSTART:20260101T000700Z RRULE:FREQ=MINUTELY;INTERVAL=1",
		WantCron:  "* * * * *",
		WantFires: []string{"2026-01-01 00:01", "2026-01-01 00:02", "2026-01-01 00:03"},
	}, {
		Name:      "an hourly rule keeps its minute and needs no hour offset",
		RRule:     "DTSTART:20260101T001500Z RRULE:FREQ=HOURLY;INTERVAL=1",
		WantCron:  "15 * * * *",
		WantFires: []string{"2026-01-01 00:15", "2026-01-01 01:15", "2026-01-01 02:15"},
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			got, ok := RRULEToCron(test.RRule)
			if !ok {
				t.Fatalf("the rule was refused, so a schedule an operator has imports as nothing: %q",
					test.RRule)
			}
			if got != test.WantCron {
				t.Errorf("cron = %q, want %q", got, test.WantCron)
			}

			// The expression has to be one the product's own scheduler accepts and fires where the rule
			// says. A converter that emits a shape the validator refuses has moved the failure rather
			// than fixed it.
			sc := &schedule.Schedule{ID: "sch_1", Name: "n", Cron: got, TemplateID: "tpl_1",
				Enabled: true}
			if err := sc.Validate(); err != nil {
				t.Fatalf("the product refuses its own converted expression %q: %v", got, err)
			}
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			for _, want := range test.WantFires {
				next, err := sc.NextFire(at)
				if err != nil {
					t.Fatalf("NextFire() error = %v", err)
				}
				if fired := next.UTC().Format("2006-01-02 15:04"); fired != want {
					t.Errorf("fires at %s, want %s (rule %q became %q)", fired, want, test.RRule, got)
				}
				at = next
			}
		})
	}
}

// TestARuleThatIsMoreThanOneRuleIsRefused covers a recurrence read as though half of it were not there.
//
// AWX puts the whole recurrence in one field, and iCalendar allows more than one RRULE in it plus EXRULE
// and EXDATE to take dates back out. The parser folded every RRULE line into one map, so the last
// line's keys quietly won, and exclusions were never read: a window that excludes a holiday imported
// firing on the holiday. Both are refused as cron, which is what sends them across as a recurrence.
func TestARuleThatIsMoreThanOneRuleIsRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case is.
		Name string
		// RRule is the recurrence field.
		RRule string
	}{{
		Name: "two recurrence rules",
		RRule: "DTSTART:20260101T020000Z RRULE:FREQ=WEEKLY;BYDAY=MO " +
			"RRULE:FREQ=WEEKLY;BYDAY=TH",
	}, {
		Name:  "an exclusion rule",
		RRule: "DTSTART:20260101T020000Z RRULE:FREQ=DAILY EXRULE:FREQ=WEEKLY;BYDAY=SA,SU",
	}, {
		Name:  "excluded dates",
		RRule: "DTSTART:20260101T020000Z RRULE:FREQ=DAILY EXDATE:20261225T020000Z",
	}, {
		Name:  "excluded dates with a zone parameter",
		RRule: "DTSTART:20260101T020000Z RRULE:FREQ=DAILY EXDATE;TZID=America/Chicago:20261225T020000",
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			if got, ok := RRULEToCron(test.RRule); ok {
				t.Errorf("the rule converted to %q, which is one cadence standing in for a recurrence "+
					"that is not one. A schedule firing on the dates its own rule removed is worse "+
					"than one that did not import.", got)
			}
		})
	}
}
