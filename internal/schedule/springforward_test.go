package schedule

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// walkLocal drives a schedule's NextFire the way the tick loop does, from the given time, and
// renders each fire on the zone's wall clock to the minute, so a table can say what a person reads.
func walkLocal(t *testing.T, sc *Schedule, from time.Time, loc *time.Location, n int) []string {
	t.Helper()
	at, out := from, make([]string, 0, n)
	for range n {
		next, err := sc.NextFire(at)
		if err != nil {
			t.Fatalf("NextFire(cron %q rrule %q setting %q) error = %v", sc.Cron, sc.RRule,
				sc.SpringForward, err)
		}
		if !next.After(at) {
			t.Fatalf("NextFire returned %v, which is not after %v: the loop would spin", next, at)
		}
		out = append(out, next.In(loc).Format("2006-01-02 15:04 MST"))
		at = next
	}
	return out
}

// TestCronSpringForwardSettings pins what each setting does with a cron slot the clocks skip, in a
// zone that jumps an hour and one that jumps half an hour, and that an empty setting is the jump a
// cron schedule has always made.
func TestCronSpringForwardSettings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantFires []string
		Zone      string
		Cron      string
		Setting   string
		From      string
	}{{ // Test 0: New York jumps 02:00 to 03:00 on 2026-03-08; jump fires at 03:00.
		Zone: "America/New_York", Cron: "30 2 * * *", Setting: SpringForwardJump,
		From:      "2026-03-07T03:00:00",
		WantFires: []string{"2026-03-08 03:00 EDT", "2026-03-09 02:30 EDT"},
	}, { // Test 1: Later fires the length of the jump later, at 03:30.
		Zone: "America/New_York", Cron: "30 2 * * *", Setting: SpringForwardLater,
		From:      "2026-03-07T03:00:00",
		WantFires: []string{"2026-03-08 03:30 EDT", "2026-03-09 02:30 EDT"},
	}, { // Test 2: Skip does not fire that night at all.
		Zone: "America/New_York", Cron: "30 2 * * *", Setting: SpringForwardSkip,
		From:      "2026-03-07T03:00:00",
		WantFires: []string{"2026-03-09 02:30 EDT", "2026-03-10 02:30 EDT"},
	}, { // Test 3: No setting is the cron default, the jump.
		Zone: "America/New_York", Cron: "30 2 * * *", From: "2026-03-07T03:00:00",
		WantFires: []string{"2026-03-08 03:00 EDT", "2026-03-09 02:30 EDT"},
	}, { // Test 4: Lord Howe jumps 02:00 to 02:30 on 2026-10-04; jump fires at 02:30.
		Zone: "Australia/Lord_Howe", Cron: "15 2 * * *", Setting: SpringForwardJump,
		From:      "2026-10-03T03:00:00",
		WantFires: []string{"2026-10-04 02:30 +11", "2026-10-05 02:15 +11"},
	}, { // Test 5: Later reads 02:15 with the half-hour offset before the jump: 02:45.
		Zone: "Australia/Lord_Howe", Cron: "15 2 * * *", Setting: SpringForwardLater,
		From:      "2026-10-03T03:00:00",
		WantFires: []string{"2026-10-04 02:45 +11", "2026-10-05 02:15 +11"},
	}, { // Test 6: Skip misses the night there too.
		Zone: "Australia/Lord_Howe", Cron: "15 2 * * *", Setting: SpringForwardSkip,
		From:      "2026-10-03T03:00:00",
		WantFires: []string{"2026-10-05 02:15 +11", "2026-10-06 02:15 +11"},
	}, { // Test 7: London jumps 01:00 to 02:00 on 2026-03-29; later fires at 02:30.
		Zone: "Europe/London", Cron: "30 1 * * *", Setting: SpringForwardLater,
		From:      "2026-03-28T03:00:00",
		WantFires: []string{"2026-03-29 02:30 BST", "2026-03-30 01:30 BST"},
	}, { // Test 8: A slot outside the jump is untouched by any setting.
		Zone: "America/New_York", Cron: "0 5 * * *", Setting: SpringForwardSkip,
		From:      "2026-03-07T06:00:00",
		WantFires: []string{"2026-03-08 05:00 EDT", "2026-03-09 05:00 EDT"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			loc := mustZone(test.Zone)
			from, err := time.ParseInLocation("2006-01-02T15:04:05", test.From, loc)
			if err != nil {
				t.Fatalf("ParseInLocation() error = %v", err)
			}
			sc := &Schedule{Cron: test.Cron, Timezone: test.Zone, SpringForward: test.Setting,
				Playbook: "site.yml"}
			if err := sc.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			got := walkLocal(t, sc, from, loc, len(test.WantFires))
			if diff := cmp.Diff(test.WantFires, got); diff != "" {
				t.Errorf("fires mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestCronLaterFiresEverySkippedSlot pins that several slots inside one jump each fire under the
// later setting, one after another, and that none of them is lost once the first has fired.
//
// The scheduler asks for the next fire after the moment it fired, which is already past the jump by
// the time the second skipped slot is due, so a correction that only looked between the asked-about
// time and the cron library's answer found the first slot and dropped the other three.
func TestCronLaterFiresEverySkippedSlot(t *testing.T) {
	t.Parallel()
	ny := mustZone("America/New_York")
	tests := []struct {
		WantFires []string
		Cron      string
		Setting   string
	}{{ // Test 0: Four slots in the jump fire at 03:00, 03:15, 03:30, and 03:45.
		Cron: "*/15 2 * * *", Setting: SpringForwardLater,
		WantFires: []string{
			"2026-03-08 03:00 EDT", "2026-03-08 03:15 EDT", "2026-03-08 03:30 EDT",
			"2026-03-08 03:45 EDT", "2026-03-09 02:00 EDT",
		},
	}, { // Test 1: Under jump they are one fire at the jump.
		Cron: "*/15 2 * * *", Setting: SpringForwardJump,
		WantFires: []string{"2026-03-08 03:00 EDT", "2026-03-09 02:00 EDT", "2026-03-09 02:15 EDT"},
	}, { // Test 2: A skipped slot that lands on a real one fires once, never twice.
		Cron: "0,30 2,3 * * *", Setting: SpringForwardLater,
		WantFires: []string{
			"2026-03-08 03:00 EDT", "2026-03-08 03:30 EDT", "2026-03-09 02:00 EDT",
			"2026-03-09 02:30 EDT", "2026-03-09 03:00 EDT",
		},
	}, { // Test 3: The same under jump: the jump is the real 03:00.
		Cron: "0,30 2,3 * * *", Setting: SpringForwardJump,
		WantFires: []string{
			"2026-03-08 03:00 EDT", "2026-03-08 03:30 EDT", "2026-03-09 02:00 EDT",
		},
	}, { // Test 4: Hourly: the skipped 02:00 lands on 03:00 and fires once.
		Cron: "0 * * * *", Setting: SpringForwardLater,
		WantFires: []string{"2026-03-08 01:00 EST", "2026-03-08 03:00 EDT", "2026-03-08 04:00 EDT"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			sc := &Schedule{Cron: test.Cron, Timezone: "America/New_York",
				SpringForward: test.Setting, Playbook: "site.yml"}
			from := time.Date(2026, 3, 8, 0, 30, 0, 0, ny)
			got := walkLocal(t, sc, from, ny, len(test.WantFires))
			if diff := cmp.Diff(test.WantFires, got); diff != "" {
				t.Errorf("fires mismatch (-want +got):\n%s", diff)
			}
			// The scheduler asks after a tick, a few seconds past the fire, and on a server that
			// runs in UTC. Neither may change the answer.
			late := walkFromTicks(t, sc, from.UTC(), ny, len(test.WantFires))
			if diff := cmp.Diff(test.WantFires, late); diff != "" {
				t.Errorf("fires asked the way the scheduler asks mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// walkFromTicks is walkLocal asked the way the scheduler asks: each next fire is computed from a
// tick seven seconds after the previous fire, in UTC.
func walkFromTicks(t *testing.T, sc *Schedule, from time.Time, loc *time.Location, n int) []string {
	t.Helper()
	at, out := from, make([]string, 0, n)
	for range n {
		next, err := sc.NextFire(at)
		if err != nil {
			t.Fatalf("NextFire() error = %v", err)
		}
		out = append(out, next.In(loc).Format("2006-01-02 15:04 MST"))
		at = next.Add(7 * time.Second).UTC()
	}
	return out
}

// TestCronCorrectionsReadTheScheduleZone pins that both daylight-saving corrections are worked out
// in the schedule's zone, whatever zone the time they are asked about is in.
//
// The scheduler asks with the server's clock. A server running in UTC, which is how a container
// runs, asked in UTC, and the corrections read the zone off that time: UTC never moves, so a
// schedule pinned to Chicago fired twice on the night the clocks went back and not at all on the
// night they went forward, while every test written in Chicago time passed.
func TestCronCorrectionsReadTheScheduleZone(t *testing.T) {
	t.Parallel()
	chicago := mustZone("America/Chicago")
	tests := []struct {
		WantFires []string
		Cron      string
		From      time.Time
	}{{ // Test 0: Spring forward asked in UTC fires at the jump, not a day late.
		Cron: "0 2 * * *", From: time.Date(2026, 3, 7, 18, 0, 0, 0, time.UTC),
		WantFires: []string{"2026-03-08 03:00 CDT", "2026-03-09 02:00 CDT"},
	}, { // Test 1: Fall back asked in UTC fires the repeated 01:30 once.
		Cron: "30 1 * * *", From: time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC),
		WantFires: []string{"2026-11-01 01:30 CDT", "2026-11-02 01:30 CST"},
	}, { // Test 2: The same asked in Tokyo, a zone with no transitions of its own.
		Cron: "30 1 * * *", From: time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC).In(
			mustZone("Asia/Tokyo")),
		WantFires: []string{"2026-11-01 01:30 CDT", "2026-11-02 01:30 CST"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			sc := &Schedule{Cron: test.Cron, Timezone: "America/Chicago", Playbook: "site.yml"}
			got := walkLocal(t, sc, test.From, chicago, len(test.WantFires))
			if diff := cmp.Diff(test.WantFires, got); diff != "" {
				t.Errorf("fires mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestFallBackIsTheSameUnderEverySetting pins that the spring-forward setting leaves the other
// night alone: a time that happens twice fires once, at the first, for a cron expression and a
// recurrence rule under every setting.
func TestFallBackIsTheSameUnderEverySetting(t *testing.T) {
	t.Parallel()
	ny := mustZone("America/New_York")
	for testNum, setting := range []string{
		"", SpringForwardJump, SpringForwardLater, SpringForwardSkip,
	} { // Tests 0 to 3: The default and each setting.
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			want := []string{"2026-11-01 01:30 EDT", "2026-11-02 01:30 EST"}
			cron := &Schedule{Cron: "30 1 * * *", Timezone: "America/New_York",
				SpringForward: setting, Playbook: "site.yml"}
			from := time.Date(2026, 10, 31, 12, 0, 0, 0, ny)
			if diff := cmp.Diff(want, walkLocal(t, cron, from, ny, 2)); diff != "" {
				t.Errorf("cron fires mismatch (-want +got):\n%s", diff)
			}
			rule := &Schedule{RRule: "DTSTART;TZID=America/New_York:20261025T013000\n" +
				"RRULE:FREQ=DAILY", SpringForward: setting, Playbook: "site.yml"}
			if diff := cmp.Diff(want, walkLocal(t, rule, from, ny, 2)); diff != "" {
				t.Errorf("rule fires mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRecurrenceSpringForwardSettings pins what each setting does with a recurrence's wall time the
// clocks skip, and that RDATE, EXDATE, and EXRULE lines are placed the same way as the rule they
// add to or take from, so an exclusion still removes the occurrence it names.
func TestRecurrenceSpringForwardSettings(t *testing.T) {
	t.Parallel()
	const daily = "DTSTART;TZID=America/New_York:20260301T023000 RRULE:FREQ=DAILY"
	tests := []struct {
		WantFires []string
		Rule      string
		Setting   string
		After     string
	}{{ // Test 0: Later is RFC 5545's reading and the default: 02:30 fires at 03:30.
		Rule: daily, Setting: SpringForwardLater, After: "2026-03-07T17:00:00Z",
		WantFires: []string{"2026-03-08T07:30:00Z", "2026-03-09T06:30:00Z"},
	}, { // Test 1: Jump fires at the instant the clock jumps, 03:00.
		Rule: daily, Setting: SpringForwardJump, After: "2026-03-07T17:00:00Z",
		WantFires: []string{"2026-03-08T07:00:00Z", "2026-03-09T06:30:00Z"},
	}, { // Test 2: Skip does not fire that night.
		Rule: daily, Setting: SpringForwardSkip, After: "2026-03-07T17:00:00Z",
		WantFires: []string{"2026-03-09T06:30:00Z", "2026-03-10T06:30:00Z"},
	}, { // Test 3: Lord Howe's half-hour jump: 02:10 under jump fires at 02:30.
		Rule:    "DTSTART;TZID=Australia/Lord_Howe:20260901T021000 RRULE:FREQ=DAILY",
		Setting: SpringForwardJump, After: "2026-10-02T16:00:00Z",
		WantFires: []string{"2026-10-03T15:30:00Z", "2026-10-04T15:10:00Z"},
	}, { // Test 4: Several times in one jump fire one after another under later.
		Rule: "DTSTART;TZID=America/New_York:20260301T020000 " +
			"RRULE:FREQ=DAILY;BYHOUR=2;BYMINUTE=0,15,30,45",
		Setting: SpringForwardLater, After: "2026-03-08T06:30:00Z",
		WantFires: []string{
			"2026-03-08T07:00:00Z", "2026-03-08T07:15:00Z", "2026-03-08T07:30:00Z",
			"2026-03-08T07:45:00Z", "2026-03-09T06:00:00Z",
		},
	}, { // Test 5: The same times are one fire at the jump under jump.
		Rule: "DTSTART;TZID=America/New_York:20260301T020000 " +
			"RRULE:FREQ=DAILY;BYHOUR=2;BYMINUTE=0,15,30,45",
		Setting: SpringForwardJump, After: "2026-03-08T06:30:00Z",
		WantFires: []string{"2026-03-08T07:00:00Z", "2026-03-09T06:00:00Z"},
	}, { // Test 6: An RDATE in the jump fires at the jump under jump.
		Rule: "DTSTART;TZID=America/New_York:20260302T090000 RRULE:FREQ=WEEKLY;BYDAY=MO " +
			"RDATE;TZID=America/New_York:20260308T023000",
		Setting: SpringForwardJump, After: "2026-03-07T17:00:00Z",
		WantFires: []string{"2026-03-08T07:00:00Z", "2026-03-09T13:00:00Z"},
	}, { // Test 7: Under skip the RDATE does not fire.
		Rule: "DTSTART;TZID=America/New_York:20260302T090000 RRULE:FREQ=WEEKLY;BYDAY=MO " +
			"RDATE;TZID=America/New_York:20260308T023000",
		Setting: SpringForwardSkip, After: "2026-03-07T17:00:00Z",
		WantFires: []string{"2026-03-09T13:00:00Z"},
	}, { // Test 8: An EXDATE naming the skipped time still removes it under jump.
		Rule:    daily + " EXDATE;TZID=America/New_York:20260308T023000",
		Setting: SpringForwardJump, After: "2026-03-07T17:00:00Z",
		WantFires: []string{"2026-03-09T06:30:00Z"},
	}, { // Test 9: An EXRULE taking out Sundays still removes the jump's Sunday under jump.
		Rule:    daily + " EXRULE:FREQ=WEEKLY;BYDAY=SU",
		Setting: SpringForwardJump, After: "2026-03-07T17:00:00Z",
		WantFires: []string{"2026-03-09T06:30:00Z"},
	}, { // Test 10: And under later.
		Rule:    daily + " EXRULE:FREQ=WEEKLY;BYDAY=SU",
		Setting: SpringForwardLater, After: "2026-03-07T17:00:00Z",
		WantFires: []string{"2026-03-09T06:30:00Z"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			sc := &Schedule{RRule: test.Rule, SpringForward: test.Setting, Playbook: "p.yml"}
			at, err := time.Parse(time.RFC3339, test.After)
			if err != nil {
				t.Fatalf("time.Parse() error = %v", err)
			}
			var got []string
			for range test.WantFires {
				next, err := sc.NextFire(at)
				if err != nil {
					t.Fatalf("NextFire() error = %v", err)
				}
				got = append(got, next.UTC().Format(time.RFC3339))
				at = next
			}
			if diff := cmp.Diff(test.WantFires, got); diff != "" {
				t.Errorf("fires mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestIntervalIgnoresTheSetting pins that an interval schedule, which counts real elapsed time and
// has no wall clock to lose, fires the same instants under every setting across a jump.
func TestIntervalIgnoresTheSetting(t *testing.T) {
	t.Parallel()
	ny := mustZone("America/New_York")
	from := time.Date(2026, 3, 7, 23, 0, 0, 0, ny)
	want := walkLocal(t, &Schedule{Cron: "@every 50m", Timezone: "America/New_York",
		Playbook: "p.yml"}, from, ny, 8)
	for testNum, setting := range []string{
		SpringForwardJump, SpringForwardLater, SpringForwardSkip,
	} { // Tests 0 to 2: Each setting.
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := walkLocal(t, &Schedule{Cron: "@every 50m", Timezone: "America/New_York",
				SpringForward: setting, Playbook: "p.yml"}, from, ny, 8)
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("fires mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSpringForwardSettingValidation pins which settings a schedule accepts, that NextFire refuses
// an unknown one the way Validate does, and the default each cadence takes.
func TestSpringForwardSettingValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Want        error
		Sc          Schedule
		WantSetting string
	}{{ // Test 0: A cron with no setting jumps.
		Sc: Schedule{Cron: "0 2 * * *", Playbook: "p.yml"}, WantSetting: SpringForwardJump,
	}, { // Test 1: A rule with no setting fires later, as AWX does.
		Sc: Schedule{RRule: "DTSTART:20260105T020000Z RRULE:FREQ=DAILY",
			Playbook: "p.yml"},
		WantSetting: SpringForwardLater,
	}, { // Test 2: A cron may skip.
		Sc: Schedule{Cron: "0 2 * * *", SpringForward: SpringForwardSkip,
			Playbook: "p.yml"},
		WantSetting: SpringForwardSkip,
	}, { // Test 3: A rule may jump.
		Sc: Schedule{RRule: "DTSTART:20260105T020000Z RRULE:FREQ=DAILY",
			SpringForward: SpringForwardJump, Playbook: "p.yml"},
		WantSetting: SpringForwardJump,
	}, { // Test 4: A cron may fire later.
		Sc: Schedule{Cron: "0 2 * * *", SpringForward: SpringForwardLater,
			Playbook: "p.yml"},
		WantSetting: SpringForwardLater,
	}, { // Test 5: Anything else is refused.
		Sc:   Schedule{Cron: "0 2 * * *", SpringForward: "sometimes", Playbook: "p.yml"},
		Want: ErrBadSpringForward,
	}, { // Test 6: Case is not folded: the value is one of three words.
		Sc:   Schedule{Cron: "0 2 * * *", SpringForward: "Skip", Playbook: "p.yml"},
		Want: ErrBadSpringForward,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			sc := test.Sc
			if err := sc.Validate(); !errors.Is(err, test.Want) {
				t.Fatalf("Validate() error = %v, want %v", err, test.Want)
			}
			if _, err := sc.NextFire(time.Now()); !errors.Is(err, test.Want) {
				t.Fatalf("NextFire() error = %v, want %v", err, test.Want)
			}
			if test.Want != nil {
				return
			}
			if diff := cmp.Diff(test.WantSetting, sc.SpringForwardSetting()); diff != "" {
				t.Errorf("SpringForwardSetting() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestNextSpringGap pins the night the preview describes: the first one the clocks go forward over
// a time the schedule names, what the jump erases, and where the schedule fires that night under
// its setting, or nothing at all when the setting changes nothing.
func TestNextSpringGap(t *testing.T) {
	t.Parallel()
	after := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	nyJump := time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC)
	tests := []struct {
		WantGap *SpringGap
		Sc      Schedule
	}{{ // Test 0: A nightly cron in the lost hour jumps by default.
		Sc: Schedule{Cron: "30 2 * * *", Timezone: "America/New_York", Playbook: "p.yml"},
		WantGap: &SpringGap{Transition: nyJump, Zone: "America/New_York", From: "02:00",
			To: "03:00", Setting: SpringForwardJump, Fires: []time.Time{nyJump}},
	}, { // Test 1: The same rule fires later by default.
		Sc: Schedule{RRule: "DTSTART;TZID=America/New_York:20260101T023000 RRULE:FREQ=DAILY",
			Playbook: "p.yml"},
		WantGap: &SpringGap{Transition: nyJump, Zone: "America/New_York", From: "02:00",
			To: "03:00", Setting: SpringForwardLater,
			Fires: []time.Time{nyJump.Add(30 * time.Minute)}},
	}, { // Test 2: Skip is reported with nothing firing in the hour after the jump.
		Sc: Schedule{Cron: "30 2 * * *", Timezone: "America/New_York",
			SpringForward: SpringForwardSkip, Playbook: "p.yml"},
		WantGap: &SpringGap{Transition: nyJump, Zone: "America/New_York", From: "02:00",
			To: "03:00", Setting: SpringForwardSkip},
	}, { // Test 3: An hourly cron fires the same under every setting, so nothing is reported.
		Sc: Schedule{Cron: "0 * * * *", Timezone: "America/New_York", Playbook: "p.yml"},
	}, { // Test 4: A zone that never moves has no such night.
		Sc: Schedule{Cron: "30 2 * * *", Timezone: "UTC", Playbook: "p.yml"},
	}, { // Test 5: A Monday cron misses the Sunday jumps in the window.
		Sc: Schedule{Cron: "30 2 * * 1", Timezone: "America/New_York", Playbook: "p.yml"},
	}, { // Test 6: Lord Howe's half-hour jump is reported with its own readings.
		Sc: Schedule{Cron: "15 2 * * *", Timezone: "Australia/Lord_Howe",
			SpringForward: SpringForwardLater, Playbook: "p.yml"},
		WantGap: &SpringGap{Transition: time.Date(2026, 10, 3, 15, 30, 0, 0, time.UTC),
			Zone: "Australia/Lord_Howe", From: "02:00", To: "02:30", Setting: SpringForwardLater,
			Fires: []time.Time{time.Date(2026, 10, 3, 15, 45, 0, 0, time.UTC)}},
	}, { // Test 7: A long list is cut to a dozen and counts the rest.
		Sc: Schedule{Cron: "* 2 * * *", Timezone: "America/New_York",
			SpringForward: SpringForwardLater, Playbook: "p.yml"},
		WantGap: &SpringGap{Transition: nyJump, Zone: "America/New_York", From: "02:00",
			To: "03:00", Setting: SpringForwardLater, Fires: minutesFrom(nyJump, 12),
			MoreFires: 48},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := test.Sc.NextSpringGap(after)
			if err != nil {
				t.Fatalf("NextSpringGap() error = %v", err)
			}
			// The jump a cron schedule fires at is found by a search that stops within a second of
			// it, so instants are compared to the second.
			if diff := cmp.Diff(test.WantGap, got, cmpopts.EquateEmpty(),
				cmp.Comparer(func(a, b time.Time) bool {
					return a.Truncate(time.Second).Equal(b.Truncate(time.Second))
				})); diff != "" {
				t.Errorf("NextSpringGap() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// minutesFrom returns n instants a minute apart starting at start.
func minutesFrom(start time.Time, n int) []time.Time {
	out := make([]time.Time, 0, n)
	for i := range n {
		out = append(out, start.Add(time.Duration(i)*time.Minute))
	}
	return out
}
