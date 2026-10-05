package schedule

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/teambition/rrule-go"
)

// nextFires returns the next n fires of a recurrence after the given RFC 3339 time, each rendered
// in UTC so a table can state the exact instant, and the error that stopped it early, if any.
func nextFires(t *testing.T, rule string, fallback *time.Location, after string, n int) ([]string, error) {
	t.Helper()
	rc, err := ParseRecurrence(rule, fallback)
	if err != nil {
		return nil, err
	}
	at, err := time.Parse(time.RFC3339, after)
	if err != nil {
		t.Fatalf("time.Parse(%q) error = %v", after, err)
	}
	var out []string
	for range n {
		next, err := rc.Next(at)
		if err != nil {
			return out, err
		}
		out = append(out, next.UTC().Format(time.RFC3339))
		at = next
	}
	return out, nil
}

func TestRecurrenceNext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantFires []string
		Want      error
		Rule      string
		After     string
		Fallback  *time.Location
	}{{ // Test 0: The last Friday of each quarter, which cron cannot say.
		Rule: "DTSTART;TZID=America/New_York:20260101T170000 " +
			"RRULE:FREQ=MONTHLY;BYMONTH=3,6,9,12;BYDAY=-1FR",
		After:     "2026-10-01T00:00:00Z",
		WantFires: []string{"2026-12-25T22:00:00Z", "2027-03-26T21:00:00Z", "2027-06-25T21:00:00Z"},
	}, { // Test 1: The last weekday of each quarter, by BYSETPOS over the weekdays.
		Rule: "DTSTART;TZID=America/New_York:20260101T170000 " +
			"RRULE:FREQ=MONTHLY;BYMONTH=3,6,9,12;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1",
		After:     "2026-10-01T00:00:00Z",
		WantFires: []string{"2026-12-31T22:00:00Z", "2027-03-31T21:00:00Z", "2027-06-30T21:00:00Z"},
	}, { // Test 2: Spring forward: 02:30 does not exist and fires at 03:30, the RFC 5545 reading.
		Rule:      "DTSTART;TZID=America/New_York:20260301T023000\nRRULE:FREQ=DAILY",
		After:     "2026-03-07T17:00:00Z",
		WantFires: []string{"2026-03-08T07:30:00Z", "2026-03-09T06:30:00Z"},
	}, { // Test 3: Fall back: 01:30 happens twice and fires once, at the first.
		Rule:      "DTSTART;TZID=America/New_York:20261025T013000\nRRULE:FREQ=DAILY",
		After:     "2026-10-31T16:00:00Z",
		WantFires: []string{"2026-11-01T05:30:00Z", "2026-11-02T06:30:00Z"},
	}, { // Test 4: Hourly through fall back fires each named hour once, never the repeat.
		Rule:  "DTSTART;TZID=America/New_York:20261101T000000\nRRULE:FREQ=HOURLY",
		After: "2026-11-01T04:00:00Z",
		WantFires: []string{
			"2026-11-01T05:00:00Z", "2026-11-01T07:00:00Z", "2026-11-01T08:00:00Z",
		},
	}, { // Test 5: Hourly through spring forward: the missing 02:00 and 03:00 are one fire.
		Rule:  "DTSTART;TZID=America/New_York:20260308T000000\nRRULE:FREQ=HOURLY",
		After: "2026-03-08T05:30:00Z",
		WantFires: []string{
			"2026-03-08T06:00:00Z", "2026-03-08T07:00:00Z", "2026-03-08T08:00:00Z",
		},
	}, { // Test 6: A weekly 09:00 stays 09:00 local across the change to summer time.
		Rule:  "DTSTART;TZID=Europe/London:20260302T090000 RRULE:FREQ=WEEKLY;BYDAY=MO",
		After: "2026-03-22T00:00:00Z",
		WantFires: []string{
			"2026-03-23T09:00:00Z", "2026-03-30T08:00:00Z", "2026-04-06T08:00:00Z",
		},
	}, { // Test 7: Southern hemisphere spring forward in October.
		Rule:      "DTSTART;TZID=Australia/Sydney:20260901T021500 RRULE:FREQ=DAILY",
		After:     "2026-10-02T12:00:00Z",
		WantFires: []string{"2026-10-02T16:15:00Z", "2026-10-03T16:15:00Z", "2026-10-04T15:15:00Z"},
	}, { // Test 8: A thirty-minute daylight shift moves a missing 02:10 to 02:40.
		Rule:      "DTSTART;TZID=Australia/Lord_Howe:20260901T021000 RRULE:FREQ=DAILY",
		After:     "2026-10-02T12:00:00Z",
		WantFires: []string{"2026-10-02T15:40:00Z", "2026-10-03T15:40:00Z", "2026-10-04T15:10:00Z"},
	}, { // Test 9: A minutely rule from years back keeps its phase.
		Rule:      "DTSTART:20200101T000700Z RRULE:FREQ=MINUTELY;INTERVAL=15",
		After:     "2026-10-01T10:00:00Z",
		WantFires: []string{"2026-10-01T10:07:00Z", "2026-10-01T10:22:00Z"},
	}, { // Test 10: An hourly interval from years back keeps its phase.
		Rule:      "DTSTART:20200101T010000Z RRULE:FREQ=HOURLY;INTERVAL=5",
		After:     "2026-10-01T00:00:00Z",
		WantFires: []string{"2026-10-01T01:00:00Z", "2026-10-01T06:00:00Z"},
	}, { // Test 11: An EXDATE removes one day.
		Rule: "DTSTART;TZID=America/Chicago:20260105T020000 RRULE:FREQ=DAILY " +
			"EXDATE;TZID=America/Chicago:20261003T020000",
		After:     "2026-10-01T17:00:00Z",
		WantFires: []string{"2026-10-02T07:00:00Z", "2026-10-04T07:00:00Z"},
	}, { // Test 12: An EXRULE removes the first Monday of each month from a weekday rule.
		Rule: "DTSTART;TZID=America/Chicago:20260105T020000 " +
			"RRULE:FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR EXRULE:FREQ=MONTHLY;BYDAY=1MO",
		After:     "2026-10-01T17:00:00Z",
		WantFires: []string{"2026-10-02T07:00:00Z", "2026-10-06T07:00:00Z"},
	}, { // Test 13: COUNT runs out and reports it.
		Rule:      "DTSTART;TZID=America/Chicago:20260105T020000 RRULE:FREQ=DAILY;COUNT=3",
		After:     "2026-01-05T12:00:00Z",
		WantFires: []string{"2026-01-06T08:00:00Z", "2026-01-07T08:00:00Z"},
		Want:      ErrExhausted,
	}, { // Test 14: UNTIL in UTC is inclusive.
		Rule: "DTSTART;TZID=America/Chicago:20260105T020000 " +
			"RRULE:FREQ=DAILY;UNTIL=20260107T080000Z",
		After:     "2026-01-05T12:00:00Z",
		WantFires: []string{"2026-01-06T08:00:00Z", "2026-01-07T08:00:00Z"},
		Want:      ErrExhausted,
	}, { // Test 15: An RDATE adds a one-off fire in the schedule's own zone.
		Rule:      "DTSTART:20260105T020000 RRULE:FREQ=DAILY RDATE:20260105T150000",
		After:     "2026-01-05T09:00:00Z",
		Fallback:  time.UTC,
		WantFires: []string{"2026-01-05T15:00:00Z", "2026-01-06T02:00:00Z"},
	}, { // Test 16: A floating DTSTART is read in the schedule's zone.
		Rule:      "DTSTART:20260105T090000 RRULE:FREQ=WEEKLY;BYDAY=MO",
		After:     "2026-01-04T00:00:00Z",
		Fallback:  mustZone("Asia/Tokyo"),
		WantFires: []string{"2026-01-05T00:00:00Z"},
	}, { // Test 17: Two RRULE lines merge in time order.
		Rule: "DTSTART;TZID=UTC:20260105T000000 RRULE:FREQ=WEEKLY;BYDAY=MO;BYHOUR=6 " +
			"RRULE:FREQ=WEEKLY;BYDAY=MO;BYHOUR=3",
		After:     "2026-01-05T00:00:00Z",
		WantFires: []string{"2026-01-05T03:00:00Z", "2026-01-05T06:00:00Z", "2026-01-12T03:00:00Z"},
	}, { // Test 18: A floating UNTIL is read in the DTSTART's zone, the way AWX coerces it.
		Rule: "DTSTART;TZID=America/Chicago:20260105T020000 " +
			"RRULE:FREQ=DAILY;UNTIL=20260106T020000",
		After:     "2026-01-05T12:00:00Z",
		WantFires: []string{"2026-01-06T08:00:00Z"},
		Want:      ErrExhausted,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			fallback := test.Fallback
			if fallback == nil {
				fallback = time.UTC
			}
			got, err := nextFires(t, test.Rule, fallback, test.After, len(test.WantFires)+1)
			if test.Want == nil {
				got = got[:min(len(got), len(test.WantFires))]
			}
			if !errors.Is(err, test.Want) {
				t.Fatalf("Next() error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantFires, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("fires mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// mustZone loads a zone or panics, for table literals.
func mustZone(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

func TestParseRecurrenceRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Want error
		Rule string
	}{{ // Test 0: No DTSTART says nothing about the time of day.
		Rule: "RRULE:FREQ=DAILY", Want: ErrBadRecurrence,
	}, { // Test 1: No rule at all.
		Rule: "DTSTART:20260105T020000Z", Want: ErrBadRecurrence,
	}, { // Test 2: A part RFC 5545 does not define.
		Rule: "DTSTART:20260105T020000Z RRULE:FREQ=YEARLY;BYEASTER=0", Want: ErrBadRecurrence,
	}, { // Test 3: Sub-minute cadence.
		Rule: "DTSTART:20260105T020000Z RRULE:FREQ=SECONDLY", Want: ErrBadRecurrence,
	}, { // Test 4: COUNT and UNTIL together.
		Rule: "DTSTART:20260105T020000Z RRULE:FREQ=DAILY;COUNT=2;UNTIL=20260110T000000Z",
		Want: ErrBadRecurrence,
	}, { // Test 5: An unknown zone is a timezone mistake, not a rule mistake.
		Rule: "DTSTART;TZID=Mars/Olympus_Mons:20260105T020000 RRULE:FREQ=DAILY",
		Want: ErrBadTimezone,
	}, { // Test 6: A property this scheduler does not read.
		Rule: "DTSTART:20260105T020000Z RRULE:FREQ=DAILY DURATION:PT1H", Want: ErrBadRecurrence,
	}, { // Test 7: A malformed date.
		Rule: "DTSTART:2026-01-05 RRULE:FREQ=DAILY", Want: ErrBadRecurrence,
	}, { // Test 8: A zone named twice, once by TZID and once by Z.
		Rule: "DTSTART;TZID=America/Chicago:20260105T020000Z RRULE:FREQ=DAILY",
		Want: ErrBadRecurrence,
	}, { // Test 9: An out-of-range part.
		Rule: "DTSTART:20260105T020000Z RRULE:FREQ=MONTHLY;BYMONTHDAY=32", Want: ErrBadRecurrence,
	}, { // Test 10: A part given twice.
		Rule: "DTSTART:20260105T020000Z RRULE:FREQ=DAILY;FREQ=WEEKLY", Want: ErrBadRecurrence,
	}, { // Test 11: Two DTSTART lines.
		Rule: "DTSTART:20260105T020000Z DTSTART:20260106T020000Z RRULE:FREQ=DAILY",
		Want: ErrBadRecurrence,
	}, { // Test 12: A COUNT past the bound.
		Rule: "DTSTART:20260105T020000Z RRULE:FREQ=DAILY;COUNT=10001", Want: ErrBadRecurrence,
	}, { // Test 13: A period date RDATE cannot be expanded.
		Rule: "DTSTART:20260105T020000Z RRULE:FREQ=DAILY RDATE;VALUE=PERIOD:20260105T020000Z/PT1H",
		Want: ErrBadRecurrence,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if _, err := ParseRecurrence(test.Rule, time.UTC); !errors.Is(err, test.Want) {
				t.Errorf("ParseRecurrence(%q) error = %v, want %v", test.Rule, err, test.Want)
			}
		})
	}
}

// TestRecurrenceRefusesEndlessExclusion pins the bound on a rule whose exclusions remove everything
// it generates. Unbounded, one stored row would iterate every minute until the year 2300 on every
// scheduler tick.
func TestRecurrenceRefusesEndlessExclusion(t *testing.T) {
	t.Parallel()
	rc, err := ParseRecurrence("DTSTART:20260105T000000Z RRULE:FREQ=MINUTELY "+
		"EXRULE:FREQ=MINUTELY", time.UTC)
	if err != nil {
		t.Fatalf("ParseRecurrence() error = %v", err)
	}
	after := time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC)
	if _, err := rc.Next(after); !errors.Is(err, ErrBadRecurrence) {
		t.Errorf("Next() error = %v, want ErrBadRecurrence", err)
	}
}

// TestRecurrenceFastForwardMatchesFullExpansion proves the fast-forward that keeps a years-old
// minutely rule cheap does not move a single fire: for each rule the answer equals the one a full
// expansion from DTSTART gives.
func TestRecurrenceFastForwardMatchesFullExpansion(t *testing.T) {
	t.Parallel()
	start := time.Date(2019, 3, 6, 7, 13, 0, 0, time.UTC)
	tests := []struct {
		Rule string
	}{
		{Rule: "FREQ=MINUTELY;INTERVAL=7"},                           // Test 0: Odd minutely step.
		{Rule: "FREQ=MINUTELY;INTERVAL=45;BYHOUR=1,13"},              // Test 1: Filtered minutely.
		{Rule: "FREQ=HOURLY;INTERVAL=5;BYMINUTE=0,30"},               // Test 2: Hourly with minutes.
		{Rule: "FREQ=HOURLY;INTERVAL=7;BYDAY=SA,SU"},                 // Test 3: Weekend hourly.
		{Rule: "FREQ=DAILY;INTERVAL=3"},                              // Test 4: Every third day.
		{Rule: "FREQ=DAILY;INTERVAL=2;BYMONTH=2,3"},                  // Test 5: Seasonal daily.
		{Rule: "FREQ=WEEKLY;INTERVAL=2;BYDAY=TU,TH"},                 // Test 6: Fortnightly.
		{Rule: "FREQ=WEEKLY;INTERVAL=3;WKST=SU;BYDAY=SU,MO,SA"},      // Test 7: Week start Sunday.
		{Rule: "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1"},       // Test 8: Last weekday.
		{Rule: "FREQ=DAILY;BYHOUR=2,14;BYMINUTE=5;BYSETPOS=2"},       // Test 9: Set position.
		{Rule: "FREQ=MINUTELY;INTERVAL=11;BYMONTHDAY=1,15;BYHOUR=3"}, // Test 10: Sparse minutely.
	}
	afters := []time.Time{
		time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC),
		time.Date(2024, 2, 29, 23, 59, 0, 0, time.UTC),
		time.Date(2027, 1, 3, 0, 0, 0, 0, time.UTC),
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			text := "DTSTART:" + start.Format(rrule.DateTimeFormat) + " RRULE:" + test.Rule
			rc, err := ParseRecurrence(text, time.UTC)
			if err != nil {
				t.Fatalf("ParseRecurrence() error = %v", err)
			}
			opt, err := rrule.StrToROption(test.Rule)
			if err != nil {
				t.Fatalf("StrToROption() error = %v", err)
			}
			opt.Dtstart = start
			full, err := rrule.NewRRule(*opt)
			if err != nil {
				t.Fatalf("NewRRule() error = %v", err)
			}
			for _, after := range afters {
				want := full.After(after, false)
				got, err := rc.Next(after)
				if err != nil {
					t.Fatalf("Next(%v) error = %v", after, err)
				}
				if !got.Equal(want) {
					t.Errorf("Next(%v) = %v, want %v from a full expansion", after, got, want)
				}
			}
		})
	}
}

func TestResolveWall(t *testing.T) {
	t.Parallel()
	ny := mustZone("America/New_York")
	tests := []struct {
		WantUTC string
		Wall    time.Time
		Loc     *time.Location
	}{{ // Test 0: An ordinary wall time.
		Wall: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC), Loc: ny, WantUTC: "2026-07-01T13:00:00Z",
	}, { // Test 1: Inside the spring-forward gap, read with the offset before it.
		Wall: time.Date(2026, 3, 8, 2, 30, 0, 0, time.UTC), Loc: ny, WantUTC: "2026-03-08T07:30:00Z",
	}, { // Test 2: The first instant of the gap.
		Wall: time.Date(2026, 3, 8, 2, 0, 0, 0, time.UTC), Loc: ny, WantUTC: "2026-03-08T07:00:00Z",
	}, { // Test 3: Inside the repeated hour, the first of the two.
		Wall: time.Date(2026, 11, 1, 1, 30, 0, 0, time.UTC), Loc: ny, WantUTC: "2026-11-01T05:30:00Z",
	}, { // Test 4: UTC has no transitions.
		Wall: time.Date(2026, 3, 8, 2, 30, 0, 0, time.UTC), Loc: time.UTC,
		WantUTC: "2026-03-08T02:30:00Z",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := resolveWall(test.Wall, test.Loc).UTC().Format(time.RFC3339)
			if diff := cmp.Diff(test.WantUTC, got); diff != "" {
				t.Errorf("resolveWall() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestScheduleRecurrence(t *testing.T) {
	t.Parallel()
	after := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		WantNext string
		Want     error
		Sc       Schedule
	}{{ // Test 0: A rule with a zone fires in it.
		Sc: Schedule{RRule: "DTSTART;TZID=America/New_York:20260105T090000 " +
			"RRULE:FREQ=WEEKLY;BYDAY=MO", Playbook: "p.yml"},
		WantNext: "2026-10-05T13:00:00Z",
	}, { // Test 1: A floating rule takes the schedule's zone.
		Sc: Schedule{RRule: "DTSTART:20260105T090000 RRULE:FREQ=WEEKLY;BYDAY=MO",
			Timezone: "Europe/Berlin", Playbook: "p.yml"},
		WantNext: "2026-10-05T07:00:00Z",
	}, { // Test 2: A rule's zone and the schedule's zone that disagree are refused.
		Sc: Schedule{RRule: "DTSTART;TZID=America/New_York:20260105T090000 " +
			"RRULE:FREQ=WEEKLY;BYDAY=MO", Timezone: "Europe/Berlin", Playbook: "p.yml"},
		Want: ErrBadRecurrence,
	}, { // Test 3: Both a cron and a rule are refused.
		Sc: Schedule{Cron: "0 9 * * 1", RRule: "DTSTART:20260105T090000Z RRULE:FREQ=DAILY",
			Playbook: "p.yml"},
		Want: ErrBadRecurrence,
	}, { // Test 4: A rule that already ran out does not validate.
		Sc: Schedule{RRule: "DTSTART:20200105T090000Z RRULE:FREQ=DAILY;COUNT=2",
			Playbook: "p.yml"},
		Want: ErrExhausted,
	}, { // Test 5: A cron schedule is untouched by the new field.
		Sc:       Schedule{Cron: "0 9 * * 1", Timezone: "UTC", Playbook: "p.yml"},
		WantNext: "2026-10-05T09:00:00Z",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if err := test.Sc.Validate(); !errors.Is(err, test.Want) {
				t.Fatalf("Validate() error = %v, want %v", err, test.Want)
			}
			if test.Want != nil {
				return
			}
			next, err := test.Sc.NextFire(after)
			if err != nil {
				t.Fatalf("NextFire() error = %v", err)
			}
			if diff := cmp.Diff(test.WantNext, next.UTC().Format(time.RFC3339)); diff != "" {
				t.Errorf("NextFire() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
