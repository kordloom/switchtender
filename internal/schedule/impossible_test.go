package schedule

import (
	"errors"
	"testing"
	"time"
)

// TestImpossibleCronIsRefused pins that an expression which parses but can never come due is
// rejected rather than stored.
//
// The cron library scans five years ahead and then returns the zero time with no error. The
// scheduler treats any next-run time that is not after now as due, so a stored zero was claimed,
// fired, and rewritten to zero on every tick. At the default interval that is a run every fifteen
// seconds, forever, from one authenticated call, with nothing logged.
func TestImpossibleCronIsRefused(t *testing.T) {
	t.Parallel()
	impossible := []string{
		"0 0 30 2 *", // February the thirtieth
		"0 0 31 2 *", // February the thirty-first
		"0 0 31 4 *", // April has thirty days
		"0 0 31 6 *", // so does June
	}
	for _, spec := range impossible {
		t.Run(spec, func(t *testing.T) {
			t.Parallel()
			next, err := NextFire(spec, time.Now())
			if err == nil {
				t.Errorf("NextFire(%q) returned %v with no error; a schedule that never comes due "+
					"is read as due on every tick", spec, next)
			}
			if err != nil && !errors.Is(err, ErrBadCron) {
				t.Errorf("NextFire(%q) error = %v, want ErrBadCron", spec, err)
			}
			s := &Schedule{Cron: spec, Playbook: "site.yml"}
			if verr := s.Validate(); verr == nil {
				t.Errorf("Validate() accepted %q", spec)
			}
		})
	}
	// Ordinary expressions still work: the leap-day case is rare but real, and pinning a weekday
	// alongside an impossible day-of-month is valid cron because the two are ORed, so Mondays in
	// February still fire.
	for _, spec := range []string{"0 2 * * *", "*/15 * * * *", "0 0 29 2 *", "@daily", "0 0 30 2 1"} {
		if _, err := NextFire(spec, time.Now()); err != nil {
			t.Errorf("NextFire(%q) refused a valid expression: %v", spec, err)
		}
	}
}
