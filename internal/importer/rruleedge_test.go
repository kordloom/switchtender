package importer

import (
	"fmt"
	"strings"
	"testing"
)

// TestRRULEToCronCoversEveryFrequencyAndItsRefusals pins the conversion table and, more importantly,
// every shape that must be refused. A converter that pastes an export's fields through lets the file
// choose the cadence: a rule carrying BYMINUTE=* once became "* * * * *", a run every minute, with
// no warning at all.
//
//nolint:funlen // Test function.
func TestRRULEToCronCoversEveryFrequencyAndItsRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		RRule      string
		WantResult string
		WantOK     bool
	}{
		{Name: "minutely every minute", RRule: "RRULE:FREQ=MINUTELY",
			WantResult: "* * * * *", WantOK: true}, // Test 0.
		{Name: "minutely every 15", RRule: "RRULE:FREQ=MINUTELY;INTERVAL=15",
			WantResult: "*/15 * * * *", WantOK: true}, // Test 1.
		{Name: "minutely every 60", RRule: "RRULE:FREQ=MINUTELY;INTERVAL=60",
			WantResult: "*/60 * * * *", WantOK: true}, // Test 2: the boundary that still divides.
		{Name: "minutely uneven", RRule: "RRULE:FREQ=MINUTELY;INTERVAL=45",
			WantOK: false}, // Test 3: */45 fires at 0 and 45, not every 45 minutes.
		{Name: "minutely past the hour", RRule: "RRULE:FREQ=MINUTELY;INTERVAL=61",
			WantOK: false}, // Test 4.
		{Name: "minutely zero", RRule: "RRULE:FREQ=MINUTELY;INTERVAL=0", WantOK: false}, // Test 5.
		{Name: "minutely negative", RRule: "RRULE:FREQ=MINUTELY;INTERVAL=-1",
			WantOK: false}, // Test 6.
		{Name: "minutely nonsense interval", RRule: "RRULE:FREQ=MINUTELY;INTERVAL=x",
			WantOK: false}, // Test 7.
		{Name: "hourly", RRule: "DTSTART:20260101T023000Z RRULE:FREQ=HOURLY",
			WantResult: "30 * * * *", WantOK: true}, // Test 8.
		{Name: "hourly every 6", RRule: "DTSTART:20260101T000000Z RRULE:FREQ=HOURLY;INTERVAL=6",
			WantResult: "0 */6 * * *", WantOK: true}, // Test 9.
		{Name: "hourly every 5 is uneven", RRule: "RRULE:FREQ=HOURLY;INTERVAL=5",
			WantOK: false}, // Test 10: 24 is not divisible by 5.
		{Name: "daily", RRule: "DTSTART:20260101T020000Z RRULE:FREQ=DAILY",
			WantResult: "0 2 * * *", WantOK: true}, // Test 11.
		{Name: "daily with weekdays", RRule: "DTSTART:20260101T020000Z " +
			"RRULE:FREQ=DAILY;BYDAY=MO,TU,WE,TH,FR",
			WantResult: "0 2 * * 1,2,3,4,5", WantOK: true}, // Test 12.
		{Name: "daily every other day", RRule: "RRULE:FREQ=DAILY;INTERVAL=2",
			WantOK: false}, // Test 13.
		{Name: "daily unknown day code", RRule: "RRULE:FREQ=DAILY;BYDAY=XX",
			WantOK: false}, // Test 14.
		{Name: "weekly names its day", RRule: "DTSTART:20260101T020000Z RRULE:FREQ=WEEKLY;BYDAY=SU",
			WantResult: "0 2 * * 0", WantOK: true}, // Test 15.
		{Name: "weekly falls back to dtstart", RRule: "DTSTART:20260105T020000Z RRULE:FREQ=WEEKLY",
			WantResult: "0 2 * * 1", WantOK: true}, // Test 16: 2026-01-05 is a Monday.
		{Name: "weekly with no usable dtstart", RRule: "RRULE:FREQ=WEEKLY",
			WantOK: false}, // Test 17: widening to every day turned a weekly job into a nightly one.
		{Name: "weekly every other week", RRule: "DTSTART:20260105T020000Z " +
			"RRULE:FREQ=WEEKLY;INTERVAL=2", WantOK: false}, // Test 18.
		{Name: "monthly", RRule: "DTSTART:20260101T030000Z RRULE:FREQ=MONTHLY;BYMONTHDAY=15",
			WantResult: "0 3 15 * *", WantOK: true}, // Test 19.
		{Name: "monthly day 1", RRule: "RRULE:FREQ=MONTHLY;BYMONTHDAY=1",
			WantResult: "0 0 1 * *", WantOK: true}, // Test 20.
		{Name: "monthly day 31", RRule: "RRULE:FREQ=MONTHLY;BYMONTHDAY=31",
			WantResult: "0 0 31 * *", WantOK: true}, // Test 21: the top of the range.
		{Name: "monthly day 0", RRule: "RRULE:FREQ=MONTHLY;BYMONTHDAY=0", WantOK: false},   // Test 22.
		{Name: "monthly day 32", RRule: "RRULE:FREQ=MONTHLY;BYMONTHDAY=32", WantOK: false}, // Test 23.
		{Name: "monthly without a day", RRule: "RRULE:FREQ=MONTHLY", WantOK: false},        // Test 24.
		{Name: "monthly every other month", RRule: "RRULE:FREQ=MONTHLY;BYMONTHDAY=1;INTERVAL=2",
			WantOK: false}, // Test 25.
		{Name: "yearly", RRule: "RRULE:FREQ=YEARLY", WantOK: false},      // Test 26.
		{Name: "no frequency", RRule: "RRULE:INTERVAL=1", WantOK: false}, // Test 27.
		{Name: "empty", RRule: "", WantOK: false},                        // Test 28.
		{Name: "no RRULE prefix", RRule: "FREQ=DAILY", WantOK: false},    // Test 29.
		{Name: "count bounds it", RRule: "RRULE:FREQ=MINUTELY;COUNT=1",
			WantOK: false}, // Test 30: AWX writes this for "run once".
		{Name: "until bounds it", RRule: "RRULE:FREQ=DAILY;UNTIL=20270101T000000Z",
			WantOK: false}, // Test 31.
		{Name: "byminute star", RRule: "RRULE:FREQ=DAILY;BYMINUTE=*", WantOK: false},    // Test 32.
		{Name: "byhour star", RRule: "RRULE:FREQ=DAILY;BYHOUR=*", WantOK: false},        // Test 33.
		{Name: "byminute list", RRule: "RRULE:FREQ=DAILY;BYMINUTE=0,30", WantOK: false}, // Test 34.
		{Name: "byminute step", RRule: "RRULE:FREQ=DAILY;BYHOUR=*/2", WantOK: false},    // Test 35.
		{Name: "byminute out of range", RRule: "RRULE:FREQ=DAILY;BYMINUTE=60",
			WantOK: false}, // Test 36.
		{Name: "byhour out of range", RRule: "RRULE:FREQ=DAILY;BYHOUR=24", WantOK: false}, // Test 37.
		{Name: "byhour and byminute", RRule: "RRULE:FREQ=DAILY;BYHOUR=5;BYMINUTE=7",
			WantResult: "7 5 * * *", WantOK: true}, // Test 38.
		{Name: "lowercase keys", RRule: "rrule:freq=DAILY", WantOK: false}, // Test 39: the tag is
		// matched case sensitively, so a lowercase rrule tag is not a rule.
		{Name: "keys are upper cased", RRule: "RRULE:freq=DAILY;byhour=4",
			WantResult: "0 4 * * *", WantOK: true}, // Test 40.
		{Name: "newline separated", RRule: "DTSTART:20260101T020000Z\nRRULE:FREQ=DAILY",
			WantResult: "0 2 * * *", WantOK: true}, // Test 41.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got, ok := RRULEToCron(test.RRule)
			if ok != test.WantOK {
				t.Fatalf("RRULEToCron(%q) ok = %v, want %v (got %q)",
					test.RRule, ok, test.WantOK, got)
			}
			if ok && got != test.WantResult {
				t.Errorf("RRULEToCron(%q) = %q, want %q", test.RRule, got, test.WantResult)
			}
			if !ok && got != "" {
				t.Errorf("RRULEToCron(%q) refused but returned %q, want empty", test.RRule, got)
			}
		})
	}
}

// TestRRULEProblemNamesTheRightRemedy pins that a skipped schedule tells the operator what to do.
// A rule that bounds itself needs a different fix from a cadence cron cannot express, and a cron
// entry created from a bounded rule would fire forever.
func TestRRULEProblemNamesTheRightRemedy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		RRule        string
		WantFragment string
	}{
		{Name: "count", RRule: "RRULE:FREQ=MINUTELY;COUNT=1",
			WantFragment: "runs a fixed number of times"}, // Test 0.
		{Name: "until", RRule: "RRULE:FREQ=DAILY;UNTIL=20270101T000000Z",
			WantFragment: "stops on a date"}, // Test 1.
		{Name: "count wins over until", RRule: "RRULE:FREQ=DAILY;COUNT=3;UNTIL=20270101T000000Z",
			WantFragment: "runs a fixed number of times"}, // Test 2.
		{Name: "neither", RRule: "RRULE:FREQ=YEARLY",
			WantFragment: "cadence cannot be expressed as cron"}, // Test 3.
		{Name: "empty", RRule: "",
			WantFragment: "cadence cannot be expressed as cron"}, // Test 4.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := rruleProblem(test.RRule); !strings.Contains(got, test.WantFragment) {
				t.Errorf("rruleProblem(%q) = %q, want it to mention %q",
					test.RRule, got, test.WantFragment)
			}
		})
	}
}

// TestDTSTARTTimeDefaultsToMidnightOnlyWhenThereIsNoUsableOne pins the clock reader. Defaulting
// straight to midnight when a DTSTART is present but oddly written would silently move a nightly
// job, which is the failure this whole path exists to avoid.
func TestDTSTARTTimeDefaultsToMidnightOnlyWhenThereIsNoUsableOne(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		RRule      string
		WantHour   string
		WantMinute string
	}{
		{Name: "zulu", RRule: "DTSTART:20260101T020500Z", WantHour: "2", WantMinute: "5"}, // Test 0.
		{Name: "tzid", RRule: "DTSTART;TZID=America/New_York:20260101T143000",
			WantHour: "14", WantMinute: "30"}, // Test 1.
		{Name: "midnight", RRule: "DTSTART:20260101T000000Z",
			WantHour: "0", WantMinute: "0"}, // Test 2: a single zero survives the zero trim.
		{Name: "lowercase separator", RRule: "DTSTART:20260101t0230",
			WantHour: "2", WantMinute: "30"}, // Test 3.
		{Name: "no dtstart", RRule: "RRULE:FREQ=DAILY", WantHour: "0", WantMinute: "0"},   // Test 4.
		{Name: "no colon", RRule: "DTSTART20260101T0200", WantHour: "0", WantMinute: "0"}, // Test 5.
		{Name: "no time separator", RRule: "DTSTART:20260101",
			WantHour: "0", WantMinute: "0"}, // Test 6.
		{Name: "clock too short", RRule: "DTSTART:20260101T02x",
			WantHour: "0", WantMinute: "0"}, // Test 7.
		{Name: "non digits", RRule: "DTSTART:20260101Taabbcc",
			WantHour: "0", WantMinute: "0"}, // Test 8.
		{Name: "second dtstart is not read", RRule: "DTSTART:20260101T0300Z DTSTART:20260101T0400Z",
			WantHour: "3", WantMinute: "0"}, // Test 9: the first usable one wins.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			gotHour, gotMinute := dtstartTime(test.RRule)
			if gotHour != test.WantHour || gotMinute != test.WantMinute {
				t.Errorf("dtstartTime(%q) = %q, %q, want %q, %q",
					test.RRule, gotHour, gotMinute, test.WantHour, test.WantMinute)
			}
		})
	}
}

// TestDTSTARTWeekdayIsTheDayAWeeklyRuleRepeatsOn pins the fallback a weekly rule with no BYDAY
// needs. Reading the empty set as "every day" turned a weekly patch window into a nightly one,
// reported as a clean conversion.
func TestDTSTARTWeekdayIsTheDayAWeeklyRuleRepeatsOn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		RRule   string
		WantDay string
		WantOK  bool
	}{
		{Name: "sunday", RRule: "DTSTART:20260104T020000Z", WantDay: "0", WantOK: true},   // Test 0.
		{Name: "monday", RRule: "DTSTART:20260105T020000Z", WantDay: "1", WantOK: true},   // Test 1.
		{Name: "saturday", RRule: "DTSTART:20260103T020000Z", WantDay: "6", WantOK: true}, // Test 2.
		{Name: "leap day", RRule: "DTSTART:20240229T020000Z", WantDay: "4", WantOK: true}, // Test 3.
		{Name: "tzid form", RRule: "DTSTART;TZID=UTC:20260105T020000", WantDay: "1",
			WantOK: true}, // Test 4.
		{Name: "no dtstart", RRule: "RRULE:FREQ=WEEKLY", WantOK: false},             // Test 5.
		{Name: "too short", RRule: "DTSTART:2026010", WantOK: false},                // Test 6.
		{Name: "not a date", RRule: "DTSTART:notadateT0200", WantOK: false},         // Test 7.
		{Name: "impossible date", RRule: "DTSTART:20260230T020000Z", WantOK: false}, // Test 8.
		{Name: "no colon", RRule: "DTSTART20260105", WantOK: false},                 // Test 9.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got, ok := dtstartWeekday(test.RRule)
			if ok != test.WantOK {
				t.Fatalf("dtstartWeekday(%q) ok = %v, want %v", test.RRule, ok, test.WantOK)
			}
			if ok && got != test.WantDay {
				t.Errorf("dtstartWeekday(%q) = %q, want %q", test.RRule, got, test.WantDay)
			}
		})
	}
}

// TestDTSTARTZoneReadsTheZoneOrLeavesItFloating pins the reader that keeps an imported 2am window
// firing at 2am where the operator lives. Dropping the zone moved every maintenance window by the
// offset between it and the server's, silently, with the same cron expression to look at.
func TestDTSTARTZoneReadsTheZoneOrLeavesItFloating(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		RRule    string
		WantZone string
	}{
		{Name: "tzid", RRule: "DTSTART;TZID=America/New_York:20260101T020000 RRULE:FREQ=DAILY",
			WantZone: "America/New_York"}, // Test 0.
		{Name: "zulu", RRule: "DTSTART:20260101T020000Z RRULE:FREQ=DAILY",
			WantZone: "UTC"}, // Test 1: what AWX wrote before it recorded zones.
		{Name: "floating", RRule: "DTSTART:20260101T020000 RRULE:FREQ=DAILY",
			WantZone: ""}, // Test 2: naming a zone it does not have would move the window.
		{Name: "no dtstart", RRule: "RRULE:FREQ=DAILY", WantZone: ""},              // Test 3.
		{Name: "empty tzid", RRule: "DTSTART;TZID=:20260101T020000", WantZone: ""}, // Test 4.
		{Name: "tzid without a colon", RRule: "DTSTART;TZID=UTC", WantZone: "UTC"}, // Test 5.
		{Name: "zone ending in z", RRule: "DTSTART;TZID=Foo/Baz:20260101T020000",
			WantZone: "Foo/Baz"}, // Test 6: the Z is read from the value, not the whole field.
		{Name: "value ending in z after a zone name", RRule: "DTSTART:20260101T020000z",
			WantZone: "UTC"}, // Test 7: the marker is case folded.
		{Name: "lowercase keyword", RRule: "dtstart:20260101T020000Z", WantZone: "UTC"}, // Test 8.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := dtstartZone(test.RRule); got != test.WantZone {
				t.Errorf("dtstartZone(%q) = %q, want %q", test.RRule, got, test.WantZone)
			}
		})
	}
}

// TestNumericFieldRefusesAnythingThatIsNotOneNumber pins the guard that stops an export choosing the
// cadence. A list, a range, a star, or a step pasted into a cron field is a firing pattern nobody
// asked for, so only a single number inside the field's own range is accepted.
func TestNumericFieldRefusesAnythingThatIsNotOneNumber(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Value      string
		Fallback   string
		Low, High  int
		WantResult string
		WantOK     bool
	}{
		{Name: "plain", Value: "5", Low: 0, High: 59, WantResult: "5", WantOK: true},     // Test 0.
		{Name: "low bound", Value: "0", Low: 0, High: 59, WantResult: "0", WantOK: true}, // Test 1.
		{Name: "high bound", Value: "59", Low: 0, High: 59,
			WantResult: "59", WantOK: true}, // Test 2.
		{Name: "one over", Value: "60", Low: 0, High: 59, WantOK: false}, // Test 3.
		{Name: "one under", Value: "0", Low: 1, High: 31, WantOK: false}, // Test 4.
		{Name: "leading zero normalizes", Value: "07", Low: 0, High: 23,
			WantResult: "7", WantOK: true}, // Test 5.
		{Name: "spaced", Value: "  9  ", Low: 0, High: 23,
			WantResult: "9", WantOK: true}, // Test 6.
		{Name: "fallback used", Value: "", Fallback: "3", Low: 0, High: 23,
			WantResult: "3", WantOK: true}, // Test 7.
		{Name: "no value and no fallback", Value: "", Fallback: "", Low: 0, High: 23,
			WantOK: false}, // Test 8.
		{Name: "star", Value: "*", Low: 0, High: 59, WantOK: false},           // Test 9.
		{Name: "list", Value: "0,30", Low: 0, High: 59, WantOK: false},        // Test 10.
		{Name: "range", Value: "0-30", Low: 0, High: 59, WantOK: false},       // Test 11.
		{Name: "step", Value: "*/5", Low: 0, High: 59, WantOK: false},         // Test 12.
		{Name: "negative", Value: "-1", Low: 0, High: 59, WantOK: false},      // Test 13.
		{Name: "not a number", Value: "two", Low: 0, High: 59, WantOK: false}, // Test 14.
		{Name: "plus sign", Value: "+5", Low: 0, High: 59,
			WantResult: "5", WantOK: true}, // Test 15: Atoi accepts a sign, and it is in range.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got, ok := numericField(test.Value, test.Fallback, test.Low, test.High)
			if ok != test.WantOK {
				t.Fatalf("numericField(%q, %q, %d, %d) ok = %v, want %v",
					test.Value, test.Fallback, test.Low, test.High, ok, test.WantOK)
			}
			if ok && got != test.WantResult {
				t.Errorf("numericField(%q) = %q, want %q", test.Value, got, test.WantResult)
			}
		})
	}
}

// TestCronDaysRefusesACodeItDoesNotKnow pins that an unrecognized weekday code refuses the whole
// list rather than dropping the one it could not read. A rule that fired on four days out of five
// after import would look right in the report and be wrong in the field.
func TestCronDaysRefusesACodeItDoesNotKnow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		ByDay      string
		WantResult string
		WantOK     bool
	}{
		{Name: "empty is every day", ByDay: "", WantResult: "*", WantOK: true}, // Test 0.
		{Name: "sunday", ByDay: "SU", WantResult: "0", WantOK: true},           // Test 1.
		{Name: "saturday", ByDay: "SA", WantResult: "6", WantOK: true},         // Test 2.
		{Name: "weekdays", ByDay: "MO,TU,WE,TH,FR",
			WantResult: "1,2,3,4,5", WantOK: true}, // Test 3.
		{Name: "lowercase", ByDay: "mo,fr", WantResult: "1,5", WantOK: true},  // Test 4.
		{Name: "spaced", ByDay: " MO , FR ", WantResult: "1,5", WantOK: true}, // Test 5.
		{Name: "unknown code", ByDay: "XX", WantOK: false},                    // Test 6.
		{Name: "one unknown in a list", ByDay: "MO,XX,FR", WantOK: false},     // Test 7.
		{Name: "ordinal prefix", ByDay: "2MO", WantOK: false},                 // Test 8: the
		// nth-weekday form has no cron reading, so it is refused rather than fired weekly.
		{Name: "trailing comma", ByDay: "MO,", WantOK: false}, // Test 9.
		{Name: "numeric", ByDay: "1", WantOK: false},          // Test 10.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got, ok := cronDays(test.ByDay)
			if ok != test.WantOK {
				t.Fatalf("cronDays(%q) ok = %v, want %v (got %q)", test.ByDay, ok, test.WantOK, got)
			}
			if ok && got != test.WantResult {
				t.Errorf("cronDays(%q) = %q, want %q", test.ByDay, got, test.WantResult)
			}
		})
	}
}

// TestEveryFieldAndDividesEvenly pins the two small helpers a step interval passes through. A step
// that does not divide its range produces an uneven cadence rather than the one requested, which is
// the whole reason the frequency arms refuse rather than emit "*/45".
func TestEveryFieldAndDividesEvenly(t *testing.T) {
	t.Parallel()
	stepTests := []struct {
		Interval   string
		WantResult string
	}{
		{Interval: "1", WantResult: "*"},     // Test 0.
		{Interval: "2", WantResult: "*/2"},   // Test 1.
		{Interval: "60", WantResult: "*/60"}, // Test 2.
		{Interval: "01", WantResult: "*/01"}, // Test 3: only the exact string one collapses.
	}
	for testNum, test := range stepTests {
		t.Run(fmt.Sprintf("test %d step %s", testNum, test.Interval), func(t *testing.T) {
			t.Parallel()
			if got := everyField(test.Interval); got != test.WantResult {
				t.Errorf("everyField(%q) = %q, want %q", test.Interval, got, test.WantResult)
			}
		})
	}

	divTests := []struct {
		Interval string
		Span     int
		WantOK   bool
	}{
		{Interval: "1", Span: 60, WantOK: true},   // Test 0.
		{Interval: "15", Span: 60, WantOK: true},  // Test 1.
		{Interval: "60", Span: 60, WantOK: true},  // Test 2: the boundary.
		{Interval: "61", Span: 60, WantOK: false}, // Test 3: one past it.
		{Interval: "45", Span: 60, WantOK: false}, // Test 4.
		{Interval: "0", Span: 60, WantOK: false},  // Test 5.
		{Interval: "-5", Span: 60, WantOK: false}, // Test 6.
		{Interval: "", Span: 60, WantOK: false},   // Test 7.
		{Interval: "x", Span: 60, WantOK: false},  // Test 8.
		{Interval: " 6 ", Span: 24, WantOK: true}, // Test 9.
	}
	for testNum, test := range divTests {
		t.Run(fmt.Sprintf("test %d divides %s", testNum, test.Interval), func(t *testing.T) {
			t.Parallel()
			if got := dividesEvenly(test.Interval, test.Span); got != test.WantOK {
				t.Errorf("dividesEvenly(%q, %d) = %v, want %v",
					test.Interval, test.Span, got, test.WantOK)
			}
		})
	}
}

// TestIsTwoDigitsAndTrimZero pins the two clock helpers. A clock field read wrong moves a nightly
// job by hours, and the leading zero has to go because a cron field of "02" is not what the
// scheduler expects to see stored.
func TestIsTwoDigitsAndTrimZero(t *testing.T) {
	t.Parallel()
	digitTests := []struct {
		In     string
		WantOK bool
	}{
		{In: "00", WantOK: true},   // Test 0.
		{In: "59", WantOK: true},   // Test 1.
		{In: "", WantOK: false},    // Test 2.
		{In: "1", WantOK: false},   // Test 3.
		{In: "123", WantOK: false}, // Test 4.
		{In: "1a", WantOK: false},  // Test 5.
		{In: "a1", WantOK: false},  // Test 6.
		{In: " 1", WantOK: false},  // Test 7.
		{In: "-1", WantOK: false},  // Test 8.
		{In: "٠١", WantOK: false},  // Test 9: Arabic-Indic digits are not ASCII digits.
	}
	for testNum, test := range digitTests {
		t.Run(fmt.Sprintf("test %d digits %q", testNum, test.In), func(t *testing.T) {
			t.Parallel()
			if got := isTwoDigits(test.In); got != test.WantOK {
				t.Errorf("isTwoDigits(%q) = %v, want %v", test.In, got, test.WantOK)
			}
		})
	}

	trimTests := []struct {
		In         string
		WantResult string
	}{
		{In: "00", WantResult: "0"},  // Test 0: midnight keeps a single zero.
		{In: "02", WantResult: "2"},  // Test 1.
		{In: "12", WantResult: "12"}, // Test 2.
		{In: "0", WantResult: "0"},   // Test 3.
		{In: "", WantResult: "0"},    // Test 4.
	}
	for testNum, test := range trimTests {
		t.Run(fmt.Sprintf("test %d trim %q", testNum, test.In), func(t *testing.T) {
			t.Parallel()
			if got := trimZero(test.In); got != test.WantResult {
				t.Errorf("trimZero(%q) = %q, want %q", test.In, got, test.WantResult)
			}
		})
	}
}

// TestEveryConvertedRRULEProducesAValidCronExpression pins the property that matters more than any
// single mapping: whatever this converter says is a cron expression must survive the schedule
// package's own validation. A converted cadence the scheduler refuses is a schedule that never
// exists, and a migrated nightly job that silently does not run is the worst shape this can take.
func TestEveryConvertedRRULEProducesAValidCronExpression(t *testing.T) {
	t.Parallel()
	rules := []string{
		"RRULE:FREQ=MINUTELY",
		"RRULE:FREQ=MINUTELY;INTERVAL=30",
		"DTSTART:20260101T020000Z RRULE:FREQ=HOURLY",
		"DTSTART:20260101T020000Z RRULE:FREQ=HOURLY;INTERVAL=12",
		"DTSTART:20260101T020000Z RRULE:FREQ=DAILY",
		"DTSTART:20260101T020000Z RRULE:FREQ=DAILY;BYDAY=MO,WE,FR",
		"DTSTART:20260105T020000Z RRULE:FREQ=WEEKLY",
		"DTSTART:20260101T020000Z RRULE:FREQ=WEEKLY;BYDAY=SU,SA",
		"DTSTART:20260101T233000Z RRULE:FREQ=MONTHLY;BYMONTHDAY=28",
	}
	for testNum, rule := range rules {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			spec, ok := RRULEToCron(rule)
			if !ok {
				t.Fatalf("RRULEToCron(%q) refused a rule this test expects to convert", rule)
			}
			doc := fmt.Sprintf(`{"job_templates": [{"name": "j", "playbook": "p.yml",
				"related": {"schedules": [{"name": "s", "rrule": %q}]}}]}`, rule)
			plan, err := FromAWX([]byte(doc), importNow)
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			if len(plan.Schedules) != 1 {
				t.Fatalf("schedules = %d for cron %q, want 1: the scheduler refused what the "+
					"converter produced.\nwarnings: %v", len(plan.Schedules), spec, plan.Warnings)
			}
			if plan.Schedules[0].Cron != spec {
				t.Errorf("stored cron = %q, want %q", plan.Schedules[0].Cron, spec)
			}
		})
	}
}
