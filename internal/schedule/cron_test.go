package schedule

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// TestCronVocabularyIsAcceptedOrRefusedDeliberately walks the expressions an operator is likely to
// type and pins which ones this build understands.
//
// A cron expression is unattended instruction. Accepting one this parser reads differently from the
// crontab the operator copied it from is worse than refusing it: the schedule appears, shows a
// cadence, and runs on another one. The syntaxes worth naming are the ones other schedulers do
// support, since those are the ones somebody will paste in: seconds as a sixth field, the Quartz
// last-day and nth-weekday forms, and @reboot.
func TestCronVocabularyIsAcceptedOrRefusedDeliberately(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Spec is the expression as written.
		Spec string
		// WantErr reports whether it must be refused.
		WantErr bool
	}{
		{Spec: "* * * * *"},                  // Test 0: Every minute.
		{Spec: "0 2 * * *"},                  // Test 1: Nightly.
		{Spec: "*/15 * * * *"},               // Test 2: A step.
		{Spec: "0 9-17 * * 1-5"},             // Test 3: Ranges on two fields.
		{Spec: "0 0 1,15 * *"},               // Test 4: A list.
		{Spec: "0 0 * * SUN"},                // Test 5: A weekday by name.
		{Spec: "0 0 * FEB *"},                // Test 6: A month by name.
		{Spec: "0 0 * * MON-FRI"},            // Test 7: A range of names.
		{Spec: "@daily"},                     // Test 8: A descriptor.
		{Spec: "@midnight"},                  // Test 9: Its synonym.
		{Spec: "@hourly"},                    // Test 10: Hourly.
		{Spec: "@weekly"},                    // Test 11: Weekly.
		{Spec: "@monthly"},                   // Test 12: Monthly.
		{Spec: "@yearly"},                    // Test 13: Yearly.
		{Spec: "@annually"},                  // Test 14: Its synonym.
		{Spec: "@every 1h30m"},               // Test 15: An interval.
		{Spec: "  0 2 * * *  "},              // Test 16: Surrounding whitespace is trimmed.
		{Spec: "0    2 * * *"},               // Test 17: Runs of spaces between fields.
		{Spec: "0\t2 * * *"},                 // Test 18: A tab between fields.
		{Spec: "", WantErr: true},            // Test 19: Nothing at all.
		{Spec: "   ", WantErr: true},         // Test 20: Only whitespace.
		{Spec: "not a cron", WantErr: true},  // Test 21: Words.
		{Spec: "* * * *", WantErr: true},     // Test 22: Four fields.
		{Spec: "* * * * * *", WantErr: true}, // Test 23: Six fields: seconds precision is not this
		// parser's dialect, and reading it as one would shift every field by one place.
		{Spec: "0 0 * * * *", WantErr: true}, // Test 24: The same mistake written out.
		{Spec: "@reboot", WantErr: true},     // Test 25: Not a descriptor this build has.
		{Spec: "@every", WantErr: true},      // Test 26: An interval with no duration.
		{Spec: "@every 1", WantErr: true},    // Test 27: A duration with no unit.
		{Spec: "@every abc", WantErr: true},  // Test 28: Not a duration.
		{Spec: "0 0 L * *", WantErr: true},   // Test 29: Quartz last-day, silently unsupported here.
		{Spec: "0 0 * * 5#3", WantErr: true}, // Test 30: Quartz nth-weekday.
		{Spec: "0 0 15W * *", WantErr: true}, // Test 31: Quartz nearest-weekday.
		{Spec: "0 0 * * ?"},                  // Test 32: Quartz no-specific-value, which this parser reads as any
		// value, so it means the same thing here as it does where it was copied from.
		{Spec: "60 * * * *", WantErr: true}, // Test 33: Minute sixty does not exist.
		{Spec: "* 24 * * *", WantErr: true}, // Test 34: Hour twenty-four does not exist.
		{Spec: "* * 0 * *", WantErr: true},  // Test 35: There is no day zero.
		{Spec: "* * 32 * *", WantErr: true}, // Test 36: Nor a thirty-second.
		{Spec: "* * * 0 *", WantErr: true},  // Test 37: Nor a month zero.
		{Spec: "* * * 13 *", WantErr: true}, // Test 38: Nor a thirteenth month.
		{Spec: "* * * * 7", WantErr: true},  // Test 39: Weekday seven is out of range here even
		// though some crontabs accept it as Sunday, so a paste from one of those is refused rather
		// than read as Monday.
		{Spec: "*/0 * * * *", WantErr: true},                          // Test 40: A step of zero.
		{Spec: "5-1 * * * *", WantErr: true},                          // Test 41: A backwards range.
		{Spec: "-1 * * * *", WantErr: true},                           // Test 42: A negative minute.
		{Spec: "0 0 30 2 *", WantErr: true},                           // Test 43: A date that never occurs.
		{Spec: "0 0 31 4 *", WantErr: true},                           // Test 44: April has thirty days.
		{Spec: "0 0 29 2 *"},                                          // Test 45: A leap day does occur.
		{Spec: "0 0 30 2 1"},                                          // Test 46: Day and weekday are ORed, so Mondays fire.
		{Spec: strings.Repeat("*", 5000) + " * * * *", WantErr: true}, // Test 47: A very long field.
	}
	after := time.Date(2026, 7, 5, 10, 0, 30, 0, time.UTC)
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			next, err := NextFire(test.Spec, after)
			if test.WantErr {
				if err == nil {
					t.Fatalf("NextFire(%q) = %v with no error, so an expression this build reads "+
						"differently from the crontab it came from was accepted", test.Spec, next)
				}
				if !errors.Is(err, ErrBadCron) {
					t.Errorf("NextFire(%q) error = %v, want ErrBadCron", test.Spec, err)
				}
				sc := &Schedule{Cron: test.Spec, Playbook: "site.yml"}
				if err := sc.Validate(); err == nil {
					t.Errorf("Validate() accepted %q", test.Spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("NextFire(%q) error = %v", test.Spec, err)
			}
			if !next.After(after) {
				t.Errorf("NextFire(%q) = %v, want a time after %v", test.Spec, next, after)
			}
			sc := &Schedule{Cron: test.Spec, Playbook: "site.yml"}
			if err := sc.Validate(); err != nil {
				t.Errorf("Validate() refused %q: %v", test.Spec, err)
			}
		})
	}
}

// TestLeapDayScheduleResolvesToARealFebruaryTwentyNinth pins the one date a yearly schedule can name
// that does not exist every year.
//
// It sits directly against the guard that refuses an expression which never comes due: the guard has
// to refuse February the thirtieth and let February the twenty-ninth through, and the gap between
// two leap days is four years, which is inside the five the parser will scan but not by much.
func TestLeapDayScheduleResolvesToARealFebruaryTwentyNinth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// After is where the search starts.
		After time.Time
		// WantNext is the instant the schedule must fire at.
		WantNext time.Time
	}{{ // Test 0: From an ordinary day, the next leap day is in 2028.
		After:    time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC),
		WantNext: time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC),
	}, { // Test 1: From the day before, it is tomorrow.
		After:    time.Date(2028, 2, 28, 12, 0, 0, 0, time.UTC),
		WantNext: time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC),
	}, { // Test 2: One second before the slot, it is this minute.
		After:    time.Date(2028, 2, 28, 23, 59, 59, 0, time.UTC),
		WantNext: time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC),
	}, { // Test 3: On the slot itself, the next one is four years out, which is the longest gap this
		// expression can have and still be found.
		After:    time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC),
		WantNext: time.Date(2032, 2, 29, 0, 0, 0, 0, time.UTC),
	}, { // Test 4: A 2100-adjacent search is not attempted here, because the century has no leap day
		// and the gap exceeds what the parser scans. This case pins the ordinary year instead.
		After:    time.Date(2029, 3, 1, 0, 0, 0, 0, time.UTC),
		WantNext: time.Date(2032, 2, 29, 0, 0, 0, 0, time.UTC),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := NextFire("0 0 29 2 *", test.After)
			if err != nil {
				t.Fatalf("NextFire() error = %v", err)
			}
			if !got.Equal(test.WantNext) {
				t.Errorf("NextFire() = %v, want %v", got.UTC(), test.WantNext)
			}
		})
	}
}

// TestMonthEndSchedulesLandOnTheRightDay pins the day-of-month edges either side of a short month,
// since a schedule pinned to the end of a month is how monthly billing and reporting jobs are
// written.
func TestMonthEndSchedulesLandOnTheRightDay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Spec is the expression.
		Spec string
		// After is where the search starts.
		After time.Time
		// WantNext is the instant it must fire at.
		WantNext time.Time
	}{{ // Test 0: The thirty-first skips the months that do not have one.
		Spec:  "0 0 31 * *",
		After: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		// April, June, September and November have thirty days, so the next thirty-first is in May.
		WantNext: time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC),
	}, { // Test 1: The thirtieth of a February that has twenty-eight days is skipped to the next
		// month that has a thirtieth.
		Spec:     "0 0 30 * *",
		After:    time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		WantNext: time.Date(2026, 3, 30, 0, 0, 0, 0, time.UTC),
	}, { // Test 2: The twenty-ninth of a non-leap February is skipped too.
		Spec:     "0 0 29 * *",
		After:    time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		WantNext: time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC),
	}, { // Test 3: The twenty-ninth of a leap February is not.
		Spec:     "0 0 29 * *",
		After:    time.Date(2028, 2, 1, 0, 0, 0, 0, time.UTC),
		WantNext: time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC),
	}, { // Test 4: The last minute of a year rolls into the next one.
		Spec:     "59 23 31 12 *",
		After:    time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC),
		WantNext: time.Date(2027, 12, 31, 23, 59, 0, 0, time.UTC),
	}, { // Test 5: The first minute of a year is reached from the last day of the old one.
		Spec:     "0 0 1 1 *",
		After:    time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC),
		WantNext: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := NextFire(test.Spec, test.After)
			if err != nil {
				t.Fatalf("NextFire(%q) error = %v", test.Spec, err)
			}
			if !got.Equal(test.WantNext) {
				t.Errorf("NextFire(%q) = %v, want %v", test.Spec, got.UTC(), test.WantNext)
			}
		})
	}
}

// TestUTCSpecRewritesTheZoneDescriptor pins the helper the spring-forward correction leans on.
//
// It re-reads a schedule in a zone with no transitions in order to learn the wall clock the
// expression would have picked had the clocks not moved. If it ever left a real zone in place, the
// correction would compare a zone against itself and conclude nothing was skipped, which is the
// failure that loses a nightly run once a year without saying anything.
func TestUTCSpecRewritesTheZoneDescriptor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Spec is the expression as the schedule holds it.
		Spec string
		// WantSpec is the expression read in a zone with no transitions.
		WantSpec string
	}{
		{Spec: "0 2 * * *", WantSpec: "CRON_TZ=UTC 0 2 * * *"},    // Test 0: No descriptor at all.
		{Spec: "  0 2 * * * ", WantSpec: "CRON_TZ=UTC 0 2 * * *"}, // Test 1: Whitespace is trimmed.
		{ // Test 2: A real zone is replaced.
			Spec: "CRON_TZ=America/Chicago 0 2 * * *", WantSpec: "CRON_TZ=UTC 0 2 * * *",
		},
		{ // Test 3: UTC is left as UTC.
			Spec: "CRON_TZ=UTC 0 2 * * *", WantSpec: "CRON_TZ=UTC 0 2 * * *",
		},
		{ // Test 4: A descriptor rather than fields is carried across.
			Spec: "CRON_TZ=Asia/Tokyo @daily", WantSpec: "CRON_TZ=UTC @daily",
		},
		{ // Test 5: Extra spaces after the zone are collapsed.
			Spec: "CRON_TZ=Asia/Tokyo    0 2 * * *", WantSpec: "CRON_TZ=UTC 0 2 * * *",
		},
		{ // Test 6: A bare descriptor with nothing after it is left alone rather than rewritten into
			// something the parser would read differently.
			Spec: "CRON_TZ=Asia/Tokyo", WantSpec: "CRON_TZ=Asia/Tokyo",
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := utcSpec(test.Spec); got != test.WantSpec {
				t.Errorf("utcSpec(%q) = %q, want %q", test.Spec, got, test.WantSpec)
			}
		})
	}
}

// TestSameLocalMinuteOnlyReportsAZoneRewind pins the test the fall-back guard uses to decide a fire
// is a repeat.
//
// Skipping a fire is a real cost, so the guard has to be sure. Two instants an hour apart that share
// a wall-clock minute can only be a zone that rewound; the same instant twice is not a repeat, and
// neither is any pair of instants in different minutes.
func TestSameLocalMinuteOnlyReportsAZoneRewind(t *testing.T) {
	t.Parallel()
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no tzdata available")
	}
	// 2026-11-01 01:30 happens twice in Chicago, an hour apart in absolute time.
	firstPass := time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC).In(chicago)
	secondPass := time.Date(2026, 11, 1, 7, 30, 0, 0, time.UTC).In(chicago)
	tests := []struct {
		// A and B are the two instants.
		A, B time.Time
		// WantSame is whether they share a local minute while being different instants.
		WantSame bool
	}{{ // Test 0: The repeated minute in a zone that fell back.
		A: secondPass, B: firstPass, WantSame: true,
	}, { // Test 1: The same pair the other way round.
		A: firstPass, B: secondPass, WantSame: true,
	}, { // Test 2: One instant is not a repeat of itself.
		A: firstPass, B: firstPass, WantSame: false,
	}, { // Test 3: The same wall clock a day apart is not a repeat.
		A: firstPass, B: firstPass.AddDate(0, 0, -1), WantSame: false,
	}, { // Test 4: Neighboring minutes are not a repeat.
		A: firstPass, B: firstPass.Add(-time.Minute), WantSame: false,
	}, { // Test 5: A second apart inside one minute is a repeat by this test, which is why the guard
		// is only consulted for values the cron parser produced.
		A: firstPass.Add(time.Second), B: firstPass, WantSame: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := sameLocalMinute(test.A, test.B); got != test.WantSame {
				t.Errorf("sameLocalMinute(%v, %v) = %v, want %v", test.A, test.B, got, test.WantSame)
			}
		})
	}
}

// TestTransitionInstantFindsTheMomentTheClocksMoved pins the binary search that answers when a zone
// jumped, which is the instant a schedule inside the lost hour fires at.
//
// A wrong answer here fires a nightly job at some arbitrary time on the transition day, and a zero
// answer loses the run entirely, so the search has to land on the exact second and has to report
// nothing when there was no transition to find.
func TestTransitionInstantFindsTheMomentTheClocksMoved(t *testing.T) {
	t.Parallel()
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no tzdata available")
	}
	// Chicago jumps from 01:59:59 CST to 03:00:00 CDT on 2026-03-08.
	jump := time.Date(2026, 3, 8, 8, 0, 0, 0, time.UTC)
	before := time.Date(2026, 3, 7, 12, 0, 0, 0, chicago)
	after := time.Date(2026, 3, 9, 12, 0, 0, 0, chicago)
	_, beforeOffset := before.Zone()

	// The search is second-granular by design, so it lands on the jump or within a second past it,
	// never before it: the instant it reports has to be one where the new offset is already in force.
	got := transitionInstant(before, after, chicago, beforeOffset)
	if delta := got.Sub(jump); delta < 0 || delta >= time.Second {
		t.Errorf("transitionInstant() = %v, want the jump at %v within a second", got.UTC(), jump)
	}
	// A window with no transition in it reports nothing rather than guessing.
	quiet := time.Date(2026, 7, 1, 12, 0, 0, 0, chicago)
	_, quietOffset := quiet.Zone()
	if got := transitionInstant(quiet, quiet.AddDate(0, 0, 2), chicago, quietOffset); !got.IsZero() {
		t.Errorf("transitionInstant() = %v over a window with no transition, want the zero time", got)
	}
}

// TestScheduleFiresInItsOwnZoneThroughEveryDescriptor pins that the timezone applies to whatever the
// operator wrote, not only to five-field expressions.
//
// The zone is spliced in front of the expression, so an interval or a descriptor has to survive that
// splice. A zone silently dropped means a nightly job in Tokyo running at the server's midnight,
// which is the whole failure the timezone field was added to fix.
func TestScheduleFiresInItsOwnZoneThroughEveryDescriptor(t *testing.T) {
	t.Parallel()
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skip("no tzdata available")
	}
	// 21:00 on the ninth in Tokyo, so every case below fires later the same evening or the next day.
	after := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		// Cron is the expression the schedule holds.
		Cron string
		// Timezone is the zone it is read in.
		Timezone string
		// WantLocal is the wall clock in that zone the schedule must fire at, empty when only
		// validity is being checked.
		WantLocal string
	}{{ // Test 0: Fields are read in the zone.
		Cron: "0 9 * * *", Timezone: "Asia/Tokyo", WantLocal: "2026-08-10 09:00",
	}, { // Test 1: A descriptor is read in the zone too.
		Cron: "@daily", Timezone: "Asia/Tokyo", WantLocal: "2026-08-10 00:00",
	}, { // Test 2: An interval survives the splice, though an interval has no wall clock of its own.
		Cron: "@every 1h", Timezone: "Asia/Tokyo", WantLocal: "2026-08-09 22:00",
	}, { // Test 3: A fixed-offset zone name resolves, which is what an operator who wants no daylight
		// saving at all reaches for.
		Cron: "0 9 * * *", Timezone: "Etc/GMT+5", WantLocal: "",
	}, { // Test 4: UTC resolves.
		Cron: "0 9 * * *", Timezone: "UTC", WantLocal: "",
	}, { // Test 5: The server's own zone resolves by name.
		Cron: "0 9 * * *", Timezone: "Local", WantLocal: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			sc := &Schedule{Cron: test.Cron, Timezone: test.Timezone, Playbook: "site.yml"}
			if err := sc.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			next, err := sc.NextFire(after)
			if err != nil {
				t.Fatalf("NextFire() error = %v", err)
			}
			if test.WantLocal == "" {
				return
			}
			if got := next.In(tokyo).Format("2006-01-02 15:04"); got != test.WantLocal {
				t.Errorf("NextFire() = %s in Tokyo, want %s", got, test.WantLocal)
			}
		})
	}
}

// TestATimezoneMustBeABareZoneName widens the check that a zone cannot smuggle in a cadence.
//
// The zone is spliced in front of the expression and the parser splits that descriptor at the first
// space, so any whitespace in the zone turns the rest of it into cron fields. A schedule whose
// displayed cadence is not the one it runs is the defect, so every character that could split the
// descriptor has to be refused where it is written.
func TestATimezoneMustBeABareZoneName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Timezone is the zone as stored.
		Timezone string
		// WantErr reports whether the schedule must be refused.
		WantErr bool
	}{
		{Timezone: "America/Chicago"},                        // Test 0: An ordinary zone.
		{Timezone: "UTC"},                                    // Test 1: UTC.
		{Timezone: ""},                                       // Test 2: None, meaning server local.
		{Timezone: "UTC ", WantErr: true},                    // Test 3: A trailing space.
		{Timezone: " UTC", WantErr: true},                    // Test 4: A leading space.
		{Timezone: "UTC\t* * * * *", WantErr: true},          // Test 5: A tab.
		{Timezone: "UTC\n* * * * *", WantErr: true},          // Test 6: A newline.
		{Timezone: "UTC\r* * * * *", WantErr: true},          // Test 7: A carriage return.
		{Timezone: "CRON_TZ=UTC", WantErr: true},             // Test 8: A second descriptor.
		{Timezone: "Mars/Olympus", WantErr: true},            // Test 9: A zone that does not exist.
		{Timezone: "../../etc/passwd", WantErr: true},        // Test 10: A path, not a zone.
		{Timezone: strings.Repeat("A", 5000), WantErr: true}, // Test 11: A very long name.
		{Timezone: "Zone/日本", WantErr: true},                 // Test 12: Non-ASCII.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			sc := &Schedule{Cron: "0 3 * * *", Timezone: test.Timezone, Playbook: "site.yml"}
			err := sc.Validate()
			if test.WantErr {
				if err == nil {
					next, nerr := sc.NextFire(time.Now())
					t.Fatalf("a timezone of %q was accepted and fires at %v (%v), so the cadence on "+
						"screen may not be the one it runs", test.Timezone, next, nerr)
				}
				if !errors.Is(err, ErrBadCron) {
					t.Errorf("Validate() error = %v, want ErrBadCron", err)
				}
				return
			}
			if err != nil {
				t.Errorf("Validate() refused a timezone of %q: %v", test.Timezone, err)
			}
		})
	}
}

// TestValidateRefusesBeforeItComputes pins the order the checks run in and what each one reports.
//
// The timezone is checked first because an unresolvable zone would otherwise be reported by the cron
// parser as a cron problem, and an operator who mistyped a zone would go looking at their expression.
func TestValidateRefusesBeforeItComputes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Schedule is the schedule under test.
		Schedule Schedule
		// Want is the error it must report.
		Want error
	}{{ // Test 0: A bad zone is a cron problem named as such, before the expression is even read.
		Schedule: Schedule{Cron: "not a cron", Timezone: "Mars/Olympus", Playbook: "p"},
		Want:     ErrBadCron,
	}, { // Test 1: A bad expression with a good zone is still a cron problem.
		Schedule: Schedule{Cron: "not a cron", Timezone: "UTC", Playbook: "p"}, Want: ErrBadCron,
	}, { // Test 2: A good expression with no target is a target problem, so the message points at
		// what is missing rather than at the cadence.
		Schedule: Schedule{Cron: "0 2 * * *"}, Want: ErrNoTarget,
	}, { // Test 3: A template is a target.
		Schedule: Schedule{Cron: "0 2 * * *", TemplateID: "tpl_1"}, Want: nil,
	}, { // Test 4: Steps are a target.
		Schedule: Schedule{
			Cron: "0 2 * * *", Steps: []run.PipelineStep{{Name: "one", Playbook: "one.yml"}},
		}, Want: nil,
	}, { // Test 5: A playbook is a target.
		Schedule: Schedule{Cron: "0 2 * * *", Playbook: "site.yml"}, Want: nil,
	}, { // Test 6: An inventory alone is not a target: there is nothing to run against it.
		Schedule: Schedule{Cron: "0 2 * * *", Inventory: "prod"}, Want: ErrNoTarget,
	}, { // Test 7: Shards alone are not a target either.
		Schedule: Schedule{Cron: "0 2 * * *", Shards: 4}, Want: ErrNoTarget,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			sc := test.Schedule
			err := sc.Validate()
			if !errors.Is(err, test.Want) {
				t.Errorf("Validate() error = %v, want %v", err, test.Want)
			}
		})
	}
}
