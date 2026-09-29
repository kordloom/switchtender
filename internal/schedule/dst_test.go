package schedule

import (
	"testing"
	"time"
)

// TestNextFireAcrossDaylightSaving pins what a schedule does on the two days a zone shifts.
//
// The scheduler advances from the moment it fired, so on the day a zone falls back a nightly job
// inside the repeated hour fired, advanced to the same wall-clock minute in the new offset, and
// fired again: two full executions of the same playbook on one nominal day, which for a terraform
// apply or a non-idempotent play is a real change made twice. Nothing in the repo covered either
// transition.
//
// The spring-forward gap is pinned here as the behavior it is rather than fixed. A local time that
// does not exist cannot be fired at, and choosing a substitute instant is a judgment the operator
// should make by moving the schedule off the transition hour, not one to invent silently. The test
// exists so the behavior is known and cannot change without somebody saying so.
func TestNextFireAcrossDaylightSaving(t *testing.T) {
	t.Parallel()
	tz, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no tzdata available")
	}

	// walk drives NextFire the way the tick loop does: fire, then ask for the next fire after that.
	walk := func(spec string, from time.Time, n int) []string {
		at, out := from, make([]string, 0, n)
		for range n {
			next, err := NextFire(spec, at)
			if err != nil {
				t.Fatalf("NextFire(%q) error = %v", spec, err)
			}
			out = append(out, next.In(tz).Format("2006-01-02 15:04 MST"))
			at = next
		}
		return out
	}

	t.Run("fall back fires once", func(t *testing.T) {
		t.Parallel()
		got := walk("CRON_TZ=America/Chicago 0 1 * * *",
			time.Date(2026, 10, 30, 12, 0, 0, 0, tz), 4)
		want := []string{
			"2026-10-31 01:00 CDT",
			"2026-11-01 01:00 CDT",
			"2026-11-02 01:00 CST",
			"2026-11-03 01:00 CST",
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("fire %d = %s, want %s (full sequence %v)", i, got[i], want[i], got)
			}
		}
		// The specific harm: the same nominal day must not appear twice.
		seen := map[string]bool{}
		for _, f := range got {
			day := f[:10]
			if seen[day] {
				t.Errorf("%s fired twice on one nominal day: %v", day, got)
			}
			seen[day] = true
		}
	})

	t.Run("spring forward skips the hour that does not exist", func(t *testing.T) {
		t.Parallel()
		got := walk("CRON_TZ=America/Chicago 0 2 * * *",
			time.Date(2026, 3, 6, 12, 0, 0, 0, tz), 3)
		want := []string{
			"2026-03-07 02:00 CST",
			// 2026-03-08 02:00 does not exist in this zone.
			"2026-03-09 02:00 CDT",
			"2026-03-10 02:00 CDT",
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("fire %d = %s, want %s (full sequence %v)", i, got[i], want[i], got)
			}
		}
	})

	t.Run("an hourly schedule still fires through the repeated hour", func(t *testing.T) {
		t.Parallel()
		// The repeat skip must not swallow a legitimate fire: hourly slots are distinct minutes.
		got := walk("CRON_TZ=America/Chicago 0 * * * *",
			time.Date(2026, 11, 1, 0, 30, 0, 0, tz), 4)
		if len(got) != 4 {
			t.Fatalf("got %d fires, want 4", len(got))
		}
		for i := 1; i < len(got); i++ {
			if got[i] == got[i-1] {
				t.Errorf("hourly schedule repeated a slot: %v", got)
			}
		}
	})
}
