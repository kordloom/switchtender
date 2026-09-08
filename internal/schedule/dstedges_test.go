package schedule

import (
	"fmt"
	"testing"
	"time"
)

// walkFires drives NextFire the way the tick loop does, firing and then asking for the next fire
// after the moment it fired, and returns the instants in order.
func walkFires(t *testing.T, sc *Schedule, from time.Time, n int) []time.Time {
	t.Helper()
	at, out := from, make([]time.Time, 0, n)
	for range n {
		next, err := sc.NextFire(at)
		if err != nil {
			t.Fatalf("NextFire(%q in %q) error = %v", sc.Cron, sc.Timezone, err)
		}
		if !next.After(at) {
			t.Fatalf("NextFire returned %v, which is not after %v: the loop would spin", next, at)
		}
		out = append(out, next)
		at = next
	}
	return out
}

// TestADailyScheduleFiresOnceEveryCalendarDayForAYear is the invariant both daylight saving
// corrections exist to protect, checked over a whole year rather than at two hand-picked dates.
//
// A nightly job has to run once a night. The fall-back correction stops the repeated hour firing the
// same job twice, and the spring-forward correction stops the lost hour dropping it entirely, and
// each one could be broken by a change made for the other. Walking a full year in a zone that shifts
// twice catches either regression, and the southern-hemisphere zone catches a fix that assumed the
// transitions come in the northern order.
func TestADailyScheduleFiresOnceEveryCalendarDayForAYear(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Zone is the schedule's timezone.
		Zone string
		// Hour is the hour of the daily slot, chosen to sit inside that zone's transition.
		Hour int
	}{
		{Zone: "America/Chicago", Hour: 2},  // Test 0: Springs forward over 02:00.
		{Zone: "America/Chicago", Hour: 1},  // Test 1: Falls back over 01:00.
		{Zone: "Europe/London", Hour: 1},    // Test 2: Springs forward over 01:00.
		{Zone: "Australia/Sydney", Hour: 2}, // Test 3: Southern hemisphere, transitions reversed in
		// the calendar.
		{Zone: "Pacific/Auckland", Hour: 2},  // Test 4: Another southern zone.
		{Zone: "UTC", Hour: 2},               // Test 5: A zone that never shifts, as the control.
		{Zone: "Asia/Kolkata", Hour: 2},      // Test 6: A half-hour offset with no daylight saving.
		{Zone: "America/Sao_Paulo", Hour: 0}, // Test 7: A zone that abolished daylight saving, whose
		// tzdata still carries the old transitions.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			loc, err := time.LoadLocation(test.Zone)
			if err != nil {
				t.Skip("no tzdata for " + test.Zone)
			}
			sc := &Schedule{
				Cron: fmt.Sprintf("0 %d * * *", test.Hour), Timezone: test.Zone, Playbook: "site.yml",
			}
			if err := sc.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			start := time.Date(2026, 1, 1, 12, 0, 0, 0, loc)
			fires := walkFires(t, sc, start, 365)
			seen := make(map[string]int, 365)
			for _, f := range fires {
				seen[f.In(loc).Format("2006-01-02")]++
			}
			for day, count := range seen {
				if count != 1 {
					t.Errorf("%s fired %d times in %s, want once: a nightly job ran twice on one "+
						"nominal day", day, count, test.Zone)
				}
			}
			// Every day between the first and last fire is covered, so no day was skipped either.
			first := fires[0].In(loc)
			last := fires[len(fires)-1].In(loc)
			for d := time.Date(first.Year(), first.Month(), first.Day(), 12, 0, 0, 0, loc); !d.After(last); d = d.AddDate(0, 0, 1) {
				if seen[d.Format("2006-01-02")] == 0 {
					t.Errorf("no fire on %s in %s, so a nightly job silently skipped a day",
						d.Format("2006-01-02"), test.Zone)
				}
			}
		})
	}
}

// TestAScheduleInsideTheRepeatedHourFiresOnce pins the fall-back correction from inside the hour
// that happens twice, in both hemispheres.
//
// The correction skips the second instant that shares a wall-clock minute. A job scheduled at 01:30
// on the night a northern zone rewinds, or 02:30 where a southern one does, is the case that fired
// two full executions of the same non-idempotent play on one nominal day.
func TestAScheduleInsideTheRepeatedHourFiresOnce(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Zone is the schedule's timezone.
		Zone string
		// Cron is the daily slot inside the repeated hour.
		Cron string
		// Day is the date the zone rewinds, as the schedule's own wall clock reads it.
		Day string
		// From is where the walk starts, in that zone.
		From string
	}{{ // Test 0: Chicago rewinds 02:00 to 01:00 on 2026-11-01, so 01:30 happens twice.
		Zone: "America/Chicago", Cron: "30 1 * * *", Day: "2026-11-01", From: "2026-10-30T12:00:00",
	}, { // Test 1: London rewinds 02:00 to 01:00 on 2026-10-25.
		Zone: "Europe/London", Cron: "30 1 * * *", Day: "2026-10-25", From: "2026-10-23T12:00:00",
	}, { // Test 2: Sydney rewinds 03:00 to 02:00 on 2026-04-05.
		Zone: "Australia/Sydney", Cron: "30 2 * * *", Day: "2026-04-05", From: "2026-04-03T12:00:00",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			loc, err := time.LoadLocation(test.Zone)
			if err != nil {
				t.Skip("no tzdata for " + test.Zone)
			}
			from, err := time.ParseInLocation("2006-01-02T15:04:05", test.From, loc)
			if err != nil {
				t.Fatalf("ParseInLocation() error = %v", err)
			}
			sc := &Schedule{Cron: test.Cron, Timezone: test.Zone, Playbook: "site.yml"}
			fires := walkFires(t, sc, from, 5)
			onTheDay := 0
			for _, f := range fires {
				if f.In(loc).Format("2006-01-02") == test.Day {
					onTheDay++
				}
			}
			if onTheDay != 1 {
				t.Errorf("fired %d times on %s in %s, want once: %v", onTheDay, test.Day, test.Zone,
					fires)
			}
		})
	}
}

// TestAScheduleInsideALostHalfHourStillFires pins the spring-forward correction in a zone whose
// shift is not a whole hour.
//
// Lord Howe Island moves the clock thirty minutes rather than sixty. A correction written around an
// hour would either miss the lost slot there or move a slot that was never lost, and both are the
// same silent once-a-year failure the correction exists to prevent.
func TestAScheduleInsideALostHalfHourStillFires(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("Australia/Lord_Howe")
	if err != nil {
		t.Skip("no tzdata for Australia/Lord_Howe")
	}
	// The clock goes 01:59:59 to 02:30:00 on 2026-10-04, so 02:15 does not exist that day.
	sc := &Schedule{Cron: "15 2 * * *", Timezone: "Australia/Lord_Howe", Playbook: "site.yml"}
	if err := sc.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	from := time.Date(2026, 10, 2, 12, 0, 0, 0, loc)
	fires := walkFires(t, sc, from, 4)
	var onTheDay []string
	for _, f := range fires {
		local := f.In(loc)
		if local.Format("2006-01-02") == "2026-10-04" {
			onTheDay = append(onTheDay, local.Format("15:04:05 MST"))
		}
	}
	if len(onTheDay) != 1 {
		t.Fatalf("fired %d times on the transition day, want once: %v", len(onTheDay), fires)
	}
	// The substitute instant has to be a real time on that day, at or after the slot that vanished.
	if onTheDay[0] < "02:15" {
		t.Errorf("fired at %s on the day 02:15 does not exist, want the moment the clock jumped or "+
			"later", onTheDay[0])
	}
}

// TestTheTimezoneFieldReachesTheDaylightSavingCorrections pins the wiring between the stored
// timezone and the corrections, which the existing coverage exercises only through an expression
// that already carries its own zone descriptor.
//
// A schedule stores the zone in its own field, and the expression it displays has no descriptor in
// it. If the field were ever dropped on the way to the parser, every schedule would fire in the
// server's zone and both daylight saving corrections would apply to the wrong zone, which is exactly
// the shape of failure that is invisible until the day the clocks move.
func TestTheTimezoneFieldReachesTheDaylightSavingCorrections(t *testing.T) {
	t.Parallel()
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no tzdata available")
	}
	sc := &Schedule{Cron: "0 2 * * *", Timezone: "America/Chicago", Playbook: "site.yml"}
	inline := &Schedule{Cron: "CRON_TZ=America/Chicago 0 2 * * *", Playbook: "site.yml"}
	from := time.Date(2026, 3, 6, 12, 0, 0, 0, chicago)

	field := walkFires(t, sc, from, 4)
	descriptor := walkFires(t, inline, from, 4)
	for i := range field {
		if !field[i].Equal(descriptor[i]) {
			t.Fatalf("fire %d differs: the stored timezone gave %v and the same zone written into "+
				"the expression gave %v", i, field[i], descriptor[i])
		}
	}
	// The lost hour is covered by the stored field, not only by the written descriptor.
	if got := field[1].In(chicago).Format("2006-01-02 15:04 MST"); got != "2026-03-08 03:00 CDT" {
		t.Errorf("the fire on the day 02:00 does not exist = %s, want it at the jump", got)
	}
}
