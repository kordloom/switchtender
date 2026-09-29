package importer_test

import (
	"testing"

	"github.com/kordloom/switchtender/internal/importer"
)

// TestRRULEBoundedRulesAreRefused covers the rules cron cannot hold: one with an occurrence count and
// one with an end date. AWX writes COUNT=1 for "run this once at a time I pick", and both fields were
// ignored, so that schedule imported as an unbounded cadence. With FREQ=MINUTELY, which is what AWX
// uses for a one-shot, the result was a job firing every minute forever on somebody's fleet.
func TestRRULEBoundedRulesAreRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name  string
		RRule string
	}{
		// Test 0: AWX's one-time schedule. Ignoring COUNT turned it into every minute, forever.
		{Name: "run once", RRule: "DTSTART:20260301T020000Z RRULE:FREQ=MINUTELY;INTERVAL=1;COUNT=1"},
		// Test 1: A fixed number of daily occurrences.
		{Name: "five nights", RRule: "DTSTART:20260301T020000Z RRULE:FREQ=DAILY;INTERVAL=1;COUNT=5"},
		// Test 2: A rule that stops on a date. Dropping UNTIL leaves it firing past its own end.
		{Name: "until april", RRule: "DTSTART:20260301T020000Z RRULE:FREQ=DAILY;INTERVAL=1;UNTIL=20260401T000000Z"},
	}
	for i, tc := range tests {
		if cron, ok := importer.RRULEToCron(tc.RRule); ok {
			t.Errorf("test %d (%s): RRULEToCron(%q) = %q, true; want a refusal, because cron has no "+
				"way to stop and this schedule would fire forever", i, tc.Name, tc.RRule, cron)
		}
	}

	// An unbounded rule still converts, so the refusal is about the bound and nothing else.
	if cron, ok := importer.RRULEToCron("DTSTART:20260301T020000Z RRULE:FREQ=DAILY;INTERVAL=1"); !ok ||
		cron != "0 2 * * *" {
		t.Errorf("plain nightly rule = %q, %v; want 0 2 * * *", cron, ok)
	}
}
