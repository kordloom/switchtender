package schedule

import (
	"testing"
	"time"
)

// TestATimezoneCannotSupplyTheCadence covers a schedule whose displayed cadence is not the one it runs.
//
// The timezone is spliced in front of the cron expression as a CRON_TZ descriptor, and the parser splits
// that descriptor at the first space, so everything after the zone name becomes cron fields. Nothing
// checked that the timezone was a bare zone name. A schedule could therefore be stored with an empty
// cron and a timezone of "UTC * * * * *", and it validated, computed a next fire a minute out, and fired
// every minute, while the list, the get response, the preview form, and any export of them all showed a
// blank cadence.
//
// For a product whose claim is that unattended work is legible, a stored schedule whose cadence on screen
// is not the cadence it runs is the defect, whoever wrote it. The importer already rejects a zone it
// cannot resolve, so the check exists; the write path just never applied it.
func TestATimezoneCannotSupplyTheCadence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the timezone field carried.
		Name string
		// Cron is the cadence as stored.
		Cron string
		// Timezone is the zone as stored.
		Timezone string
		// WantErr reports whether the schedule must be refused.
		WantErr bool
	}{{ // Test 0: A cadence smuggled through the zone, with the cron left blank.
		Name: "cadence in the zone", Cron: "", Timezone: "UTC * * * * *", WantErr: true,
	}, { // Test 1: The same with a descriptor the parser also accepts.
		Name: "interval in the zone", Cron: "", Timezone: "UTC @every 1s", WantErr: true,
	}, { // Test 2: A zone that overrides the cron on screen with a different one.
		Name: "zone overriding a stated cron", Cron: "0 3 * * *", Timezone: "UTC * * * * *",
		WantErr: true,
	}, { // Test 3: A second descriptor smuggled in.
		Name: "another descriptor", Cron: "0 3 * * *", Timezone: "UTC CRON_TZ=UTC", WantErr: true,
	}, { // Test 4: A zone this system cannot resolve is refused where it is written, rather than firing
		// in server time and surprising somebody later.
		Name: "unresolvable zone", Cron: "0 3 * * *", Timezone: "Mars/Olympus_Mons", WantErr: true,
	}, { // Test 5: An ordinary zone works, which is the whole feature.
		Name: "a real zone", Cron: "0 3 * * *", Timezone: "America/Chicago", WantErr: false,
	}, { // Test 6: UTC works.
		Name: "utc", Cron: "0 3 * * *", Timezone: "UTC", WantErr: false,
	}, { // Test 7: No zone at all is the server's local time, which is the default.
		Name: "no zone", Cron: "0 3 * * *", Timezone: "", WantErr: false,
	}, { // Test 8: An empty cron is refused whatever the zone says, since a schedule with no stated
		// cadence has nothing to show an operator.
		Name: "no cadence", Cron: "", Timezone: "UTC", WantErr: true,
	}}

	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			sc := &Schedule{
				ID: "sch_1", Name: "nightly", Cron: test.Cron, Timezone: test.Timezone,
				Playbook: "site.yml", Inventory: "prod", Enabled: true,
			}
			err := sc.Validate()
			if test.WantErr && err == nil {
				next, nerr := sc.NextFire(time.Now())
				t.Errorf("test %d: a schedule with cron %q and timezone %q was accepted, and fires at "+
					"%v (%v): the cadence it runs is not the one it shows",
					testNum, test.Cron, test.Timezone, next, nerr)
			}
			if !test.WantErr && err != nil {
				t.Errorf("test %d: a schedule with cron %q and timezone %q was refused: %v",
					testNum, test.Cron, test.Timezone, err)
			}
		})
	}
}
