package schedule

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestNextFireAfterSkipsTheRepeatOfWhatALateTickFired works out the fire after a late tick on the
// nights the clocks go back, in both hemispheres, and on ordinary nights.
//
// A wall clock time that happens twice fires once, at the first of the two. The scheduler used to
// work the next fire out from the tick alone, so a tick a minute or more late took the second
// reading of the time it had just fired as the next fire. The cadences that fire through the
// repeated hour, such as every five minutes, must keep doing so, and an interval or a recurrence is
// untouched.
func TestNextFireAfterSkipsTheRepeatOfWhatALateTickFired(t *testing.T) {
	t.Parallel()
	chicago := schZone(t, "America/Chicago")
	sydney := schZone(t, "Australia/Sydney")
	// 01:30 CDT on 2026-11-01, the first of the two 01:30s Chicago has that night.
	firstChicago := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC)
	// 02:30 AEDT on 2027-04-04, the first of the two 02:30s Sydney has that night.
	firstSydney := time.Date(2027, 4, 3, 15, 30, 0, 0, time.UTC)
	tests := []struct {
		Schedule Schedule
		Due      time.Time
		Now      time.Time
		Loc      *time.Location
		WantNext string
	}{{ // Test 0: A minute late in Chicago skips the second 01:30.
		Schedule: Schedule{Cron: "30 1 * * *", Timezone: "America/Chicago"},
		Due:      firstChicago, Now: firstChicago.Add(time.Minute), Loc: chicago,
		WantNext: "2026-11-02 01:30 CST",
	}, { // Test 1: Fifty-nine minutes late, just before the second 01:30, still skips it.
		Schedule: Schedule{Cron: "30 1 * * *", Timezone: "America/Chicago"},
		Due:      firstChicago, Now: firstChicago.Add(59 * time.Minute), Loc: chicago,
		WantNext: "2026-11-02 01:30 CST",
	}, { // Test 2: A tick after the clocks went back but before the second 01:30 skips it.
		Schedule: Schedule{Cron: "30 1 * * *", Timezone: "America/Chicago"},
		Due:      firstChicago, Now: firstChicago.Add(40 * time.Minute), Loc: chicago,
		WantNext: "2026-11-02 01:30 CST",
	}, { // Test 3: A tick a day late, landing after the first 01:30, skips the second.
		Schedule: Schedule{Cron: "30 1 * * *", Timezone: "America/Chicago"},
		Due:      firstChicago.Add(-24 * time.Hour), Now: firstChicago.Add(15 * time.Minute),
		Loc: chicago, WantNext: "2026-11-02 01:30 CST",
	}, { // Test 4: The same in Sydney, whose clocks go back in April.
		Schedule: Schedule{Cron: "30 2 * * *", Timezone: "Australia/Sydney"},
		Due:      firstSydney, Now: firstSydney.Add(5 * time.Minute), Loc: sydney,
		WantNext: "2027-04-05 02:30 AEST",
	}, { // Test 5: An hourly schedule fired late skips the repeat as an on-time tick does.
		Schedule: Schedule{Cron: "0 * * * *", Timezone: "America/Chicago"},
		Due:      firstChicago.Add(-30 * time.Minute), Now: firstChicago.Add(-25 * time.Minute),
		Loc: chicago, WantNext: "2026-11-01 02:00 CST",
	}, { // Test 6: Every five minutes keeps firing into the repeated hour.
		Schedule: Schedule{Cron: "*/5 * * * *", Timezone: "America/Chicago"},
		Due:      firstChicago.Add(25 * time.Minute), Now: firstChicago.Add(27 * time.Minute),
		Loc: chicago, WantNext: "2026-11-01 01:00 CST",
	}, { // Test 7: An ordinary night is unchanged.
		Schedule: Schedule{Cron: "30 1 * * *", Timezone: "America/Chicago"},
		Due:      firstChicago.Add(-14 * 24 * time.Hour),
		Now:      firstChicago.Add(-14*24*time.Hour + time.Minute),
		Loc:      chicago, WantNext: "2026-10-19 01:30 CDT",
	}, { // Test 8: An interval counts elapsed time and has no repeat to skip.
		Schedule: Schedule{Cron: "@every 30m", Timezone: "America/Chicago"},
		Due:      firstChicago, Now: firstChicago.Add(time.Minute), Loc: chicago,
		WantNext: "2026-11-01 01:01 CST",
	}, { // Test 9: A recurrence places a repeated wall time once, at the first, by itself.
		Schedule: Schedule{RRule: "DTSTART;TZID=America/Chicago:20261001T013000 RRULE:FREQ=DAILY",
			Timezone: "America/Chicago"},
		Due: firstChicago, Now: firstChicago.Add(time.Minute), Loc: chicago,
		WantNext: "2026-11-02 01:30 CST",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			next, err := test.Schedule.NextFireAfter(test.Due, test.Now)
			if err != nil {
				t.Fatalf("NextFireAfter() error = %v", err)
			}
			got := next.In(test.Loc).Format("2006-01-02 15:04 MST")
			if diff := cmp.Diff(test.WantNext, got); diff != "" {
				t.Errorf("NextFireAfter() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
