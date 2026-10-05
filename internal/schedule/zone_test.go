package schedule

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestResolveServerZoneNamesTheClocksZone resolves the server's zone name from each place it can
// come from, the way Go itself picks the zone its clock reads in.
func TestResolveServerZoneNamesTheClocksZone(t *testing.T) {
	t.Parallel()
	berlin := schZone(t, "Europe/Berlin")
	tests := []struct {
		// TZ is the TZ variable, nil when it is not set.
		TZ *string
		// Link is the target /etc/localtime links to, empty when it is not a link.
		Link string
		// File is the content of /etc/timezone, empty when there is none.
		File string
		// Local is the zone the clock reads in.
		Local *time.Location
		// WantZone is the name resolved.
		WantZone string
	}{{ // Test 0: TZ names a zone.
		TZ: ptr("America/New_York"), Link: "/usr/share/zoneinfo/Asia/Tokyo", WantZone: "America/New_York",
	}, { // Test 1: An empty TZ is UTC.
		TZ: ptr(""), Link: "/usr/share/zoneinfo/Asia/Tokyo", WantZone: UnnamedZone,
	}, { // Test 2: A leading colon is dropped.
		TZ: ptr(":Europe/Berlin"), WantZone: "Europe/Berlin",
	}, { // Test 3: A TZ that names no loadable zone leaves the clock in UTC.
		TZ: ptr("Mars/Olympus"), Link: "/usr/share/zoneinfo/Asia/Tokyo", WantZone: UnnamedZone,
	}, { // Test 4: A TZ holding a path into a zone database names that zone.
		TZ: ptr("/usr/share/zoneinfo/Asia/Tokyo"), WantZone: "Asia/Tokyo",
	}, { // Test 5: With no TZ, the zone file /etc/localtime links to.
		Link: "/usr/share/zoneinfo/America/Chicago", WantZone: "America/Chicago",
	}, { // Test 6: The layout macOS links to.
		Link: "/var/db/timezone/zoneinfo/Asia/Tokyo", WantZone: "Asia/Tokyo",
	}, { // Test 7: A relative link into the posix variant of the database.
		Link: "../usr/share/zoneinfo/posix/Europe/Paris", WantZone: "Europe/Paris",
	}, { // Test 8: A copied zone file is named by /etc/timezone when that agrees with the clock.
		File: "Europe/Berlin\n", Local: berlin, WantZone: "Europe/Berlin",
	}, { // Test 9: A stale /etc/timezone that disagrees with the clock is not believed.
		File: "Europe/Berlin\n", Local: time.UTC, WantZone: UnnamedZone,
	}, { // Test 10: Nothing names the zone.
		Local: time.UTC, WantZone: UnnamedZone,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			local := test.Local
			if local == nil {
				local = time.UTC
			}
			got := resolveServerZone(zoneSources{
				lookupEnv: func(name string) (string, bool) {
					if name != "TZ" || test.TZ == nil {
						return "", false
					}
					return *test.TZ, true
				},
				readlink: func(path string) (string, error) {
					if path != "/etc/localtime" || test.Link == "" {
						return "", os.ErrNotExist
					}
					return test.Link, nil
				},
				readFile: func(path string) ([]byte, error) {
					if path != "/etc/timezone" || test.File == "" {
						return nil, errors.New("no such file")
					}
					return []byte(test.File), nil
				},
				local: local,
			})
			if diff := cmp.Diff(test.WantZone, got); diff != "" {
				t.Errorf("resolveServerZone() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// ptr returns a pointer to s.
func ptr(s string) *string {
	return &s
}

// TestPinZoneWritesOnlyWhereNoZoneIsNamed pins a zone onto schedules that do and do not say which
// zone they are read in.
func TestPinZoneWritesOnlyWhereNoZoneIsNamed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Schedule Schedule
		WantZone string
	}{{ // Test 0: A cron expression with no zone takes the pin.
		Schedule: Schedule{Cron: "0 9 * * *"}, WantZone: "Asia/Tokyo",
	}, { // Test 1: A floating DTSTART takes the pin.
		Schedule: Schedule{RRule: "DTSTART:20260101T090000 RRULE:FREQ=DAILY"}, WantZone: "Asia/Tokyo",
	}, { // Test 2: A named timezone is kept.
		Schedule: Schedule{Cron: "0 9 * * *", Timezone: "Europe/Berlin"}, WantZone: "Europe/Berlin",
	}, { // Test 3: A CRON_TZ descriptor names the zone.
		Schedule: Schedule{Cron: "CRON_TZ=Europe/Berlin 0 9 * * *"}, WantZone: "",
	}, { // Test 4: A TZ descriptor names the zone.
		Schedule: Schedule{Cron: "TZ=Europe/Berlin 0 9 * * *"}, WantZone: "",
	}, { // Test 5: A DTSTART with a TZID names the zone.
		Schedule: Schedule{RRule: "DTSTART;TZID=Europe/Berlin:20260101T090000 RRULE:FREQ=DAILY"},
		WantZone: "",
	}, { // Test 6: A DTSTART in the Z form names UTC.
		Schedule: Schedule{RRule: "DTSTART:20260101T090000Z RRULE:FREQ=DAILY"}, WantZone: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			sc := test.Schedule
			sc.PinZone("Asia/Tokyo")
			if diff := cmp.Diff(test.WantZone, sc.Timezone); diff != "" {
				t.Errorf("PinZone() zone mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestAScheduleNamingNoZoneReadsTheSameOnEveryServer asks when a schedule that names no zone fires
// next, with the same instant handed over in the zones two servers might sit in.
//
// The cron library reads an expression with no zone in the zone of the time it is asked about, and
// the scheduler asks with its own clock's time, so each server read the schedule in its own zone.
// It is read in UnnamedZone wherever it is asked.
func TestAScheduleNamingNoZoneReadsTheSameOnEveryServer(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 6, 3, 0, 0, 0, time.UTC)
	unnamed := schZone(t, UnnamedZone)
	tests := []struct {
		Schedule Schedule
		WantWall string
	}{{ // Test 0: A cron expression.
		Schedule: Schedule{Cron: "0 9 * * *"}, WantWall: "2026-10-06 09:00",
	}, { // Test 1: A recurrence with a floating DTSTART.
		Schedule: Schedule{RRule: "DTSTART:20261001T090000 RRULE:FREQ=DAILY"},
		WantWall: "2026-10-06 09:00",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			for _, zone := range []string{"UTC", "America/Chicago", "Asia/Kolkata"} {
				next, err := test.Schedule.NextFire(at.In(schZone(t, zone)))
				if err != nil {
					t.Fatalf("NextFire() asked in %s error = %v", zone, err)
				}
				if got := next.In(unnamed).Format("2006-01-02 15:04"); got != test.WantWall {
					t.Errorf("asked in %s, next fire = %s %s, want %s", zone, got, UnnamedZone,
						test.WantWall)
				}
			}
		})
	}
}
