package schedule

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestTimezoneIsRefusedAsAZoneNameNotADescriptor covers the shape check on a schedule's timezone,
// which is the guard that stops a descriptor being smuggled in through the zone field.
//
// The zone is spliced in front of the cron expression as CRON_TZ=, and the parser splits that
// descriptor at the first space, so a timezone carrying its own separator or assignment turns the
// rest of the field into cron cadence. The stored schedule then shows one cadence and runs another.
//
// The check refuses the descriptor characters before the zone is resolved. Resolution happens to
// reject these strings too, so the two guards agree on the outcome today and only the reason differs.
// That is exactly why the reason is asserted here: with the character check gone, the refusal still
// happens and no assertion moves, so nothing records that the first guard was ever there. Pinning
// the reason keeps the ordering deliberate rather than incidental to what LoadLocation happens to
// accept.
func TestTimezoneIsRefusedAsAZoneNameNotADescriptor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Timezone is the zone the schedule declares.
		Timezone string
		// WantRefused reports whether Validate must reject it.
		WantRefused bool
		// WantShapeReason reports whether the refusal must name the zone-name shape rather than an
		// unresolvable zone, which is what proves the character check ran first.
		WantShapeReason bool
	}{{ // Test 0: A plain zone name is accepted.
		Timezone: "America/New_York", WantRefused: false,
	}, { // Test 1: An empty zone means the server's local time and is accepted.
		Timezone: "", WantRefused: false,
	}, { // Test 2: A zone with cron fields appended, the original smuggling case.
		Timezone: "UTC * * * * *", WantRefused: true, WantShapeReason: true,
	}, { // Test 3: An assignment in the zone field, which would nest a second descriptor.
		Timezone: "CRON_TZ=UTC", WantRefused: true, WantShapeReason: true,
	}, { // Test 4: A bare equals sign is a descriptor character, not part of any zone name.
		Timezone: "UTC=x", WantRefused: true, WantShapeReason: true,
	}, { // Test 5: A tab separates descriptor from expression the same way a space does.
		Timezone: "UTC\t0 0 * * *", WantRefused: true, WantShapeReason: true,
	}, { // Test 6: A newline, which would otherwise reach the parser intact.
		Timezone: "UTC\n0 0 * * *", WantRefused: true, WantShapeReason: true,
	}, { // Test 7: A well-formed name this system cannot resolve is refused for that reason instead.
		Timezone: "Mars/Olympus_Mons", WantRefused: true, WantShapeReason: false,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			sc := &Schedule{
				ID: "sch_tz", Name: "nightly", Cron: "0 3 * * *", Playbook: "site.yml",
				Inventory: "prod", Timezone: test.Timezone, Enabled: true,
			}
			err := sc.Validate()
			if !test.WantRefused {
				if err != nil {
					t.Fatalf("Validate() with timezone %q error = %v, want accepted",
						test.Timezone, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() accepted timezone %q: a descriptor in the zone field lets a "+
					"schedule display one cadence and run another", test.Timezone)
			}
			if !errors.Is(err, ErrBadCron) {
				t.Errorf("Validate() with timezone %q error = %v, want ErrBadCron",
					test.Timezone, err)
			}
			// The shape refusal names what a timezone is; the resolution refusal says it cannot be
			// resolved. Which one fires says which guard caught it.
			gotShape := strings.Contains(err.Error(), "zone name such as")
			if gotShape != test.WantShapeReason {
				t.Errorf("Validate() with timezone %q refused with %q; shape reason = %v, want %v: "+
					"a descriptor must be refused for its shape, before the zone is resolved",
					test.Timezone, err, gotShape, test.WantShapeReason)
			}
		})
	}
}
