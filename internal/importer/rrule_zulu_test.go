package importer

import "testing"

// TestAZuluDTSTARTImportsAsUTC covers the hour every imported UTC schedule fired at.
//
// An AWX schedule records the zone its window was written in, and keeping it is what makes an imported
// 2am window still fire at 2am where the operator lives. The zone was read only from a TZID parameter.
// AWX also writes DTSTART in the iCalendar Zulu form, with a trailing Z and no parameter, which is what
// every schedule had before TZID support and what a UTC schedule still gets. That form yielded no zone,
// so the schedule imported with none, which the cron reader interprets as the server's local time,
// while the hour lifted into the cron expression was the UTC one.
//
// A 09:00 UTC nightly job imported onto a Chicago server therefore fired at 09:00 Central, five or six
// hours late depending on the season, with the same cron expression on screen either way. This is the
// defect the TZID case was already fixed for, in the form that arrives most often.
func TestAZuluDTSTARTImportsAsUTC(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which DTSTART form the rule carries.
		Name string
		// RRule is the schedule's recurrence rule as AWX exported it.
		RRule string
		// WantZone is the zone the schedule must import with.
		WantZone string
	}{{ // Test 0: The Zulu form, which is UTC by definition.
		Name:     "zulu",
		RRule:    "DTSTART:20300112T090000Z RRULE:FREQ=DAILY;INTERVAL=1",
		WantZone: "UTC",
	}, { // Test 1: A lowercase trailing z, which iCalendar permits.
		Name:     "lowercase zulu",
		RRule:    "DTSTART:20300112T090000z RRULE:FREQ=DAILY;INTERVAL=1",
		WantZone: "UTC",
	}, { // Test 2: A TZID parameter still wins, since it names a real zone with its own daylight rules
		// and UTC would drop them.
		Name:     "tzid",
		RRule:    "DTSTART;TZID=America/New_York:20300112T020000 RRULE:FREQ=DAILY;INTERVAL=1",
		WantZone: "America/New_York",
	}, { // Test 3: A floating local time names no zone, which is what it means: read it where the server
		// is. Claiming UTC for it would move the window the other way.
		Name:     "floating",
		RRule:    "DTSTART:20300112T090000 RRULE:FREQ=DAILY;INTERVAL=1",
		WantZone: "",
	}, { // Test 4: A TZID whose value itself ends in a Z is a zone name, not the Zulu marker.
		Name:     "tzid ending in z",
		RRule:    "DTSTART;TZID=Africa/Lubumbashi:20300112T020000 RRULE:FREQ=DAILY",
		WantZone: "Africa/Lubumbashi",
	}}
	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			if got := dtstartZone(test.RRule); got != test.WantZone {
				t.Errorf("test %d: dtstartZone(%q) = %q, want %q: the schedule imports in the wrong "+
					"zone and fires at the wrong hour with nothing on screen showing the shift",
					testNum, test.RRule, got, test.WantZone)
			}
		})
	}
}
