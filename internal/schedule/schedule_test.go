package schedule_test

import (
	"errors"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/scheduletest"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	scheduletest.Contract(t, schedule.NewMemStore)
}

func TestNextFire(t *testing.T) {
	t.Parallel()
	if _, err := schedule.NextFire("not a cron", time.Now()); !errors.Is(err, schedule.ErrBadCron) {
		t.Errorf("NextFire() error = %v, want ErrBadCron", err)
	}

	after := time.Date(2026, 7, 5, 10, 0, 30, 0, time.UTC)
	next, err := schedule.NextFire("* * * * *", after)
	if err != nil {
		t.Fatalf("NextFire() error = %v", err)
	}
	if want := time.Date(2026, 7, 5, 10, 1, 0, 0, time.UTC); !next.Equal(want) {
		t.Errorf("NextFire() = %v, want %v", next, want)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	if err := (&schedule.Schedule{Cron: "* * * * *"}).Validate(); !errors.Is(err, schedule.ErrNoTarget) {
		t.Errorf("Validate() no target = %v, want ErrNoTarget", err)
	}
	if err := (&schedule.Schedule{Cron: "bad", Playbook: "p"}).Validate(); !errors.Is(err, schedule.ErrBadCron) {
		t.Errorf("Validate() bad cron = %v, want ErrBadCron", err)
	}
	if err := (&schedule.Schedule{Cron: "* * * * *", Playbook: "p"}).Validate(); err != nil {
		t.Errorf("Validate() valid = %v, want nil", err)
	}
}

// TestNextFireTimezone proves a schedule fires in its own timezone: the same expression comes due at
// a different absolute instant in two zones, an unset zone stays server-local, and a bad zone name is
// rejected rather than stored to misfire forever.
func TestNextFireTimezone(t *testing.T) {
	t.Parallel()
	// 09:00 daily. Reading it in Tokyo and in New York yields different absolute next-fire instants.
	after := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	tokyo := &schedule.Schedule{Cron: "0 9 * * *", Timezone: "Asia/Tokyo", Playbook: "p.yml"}
	ny := &schedule.Schedule{Cron: "0 9 * * *", Timezone: "America/New_York", Playbook: "p.yml"}
	tNext, err := tokyo.NextFire(after)
	if err != nil {
		t.Fatalf("tokyo NextFire() error = %v", err)
	}
	nNext, err := ny.NextFire(after)
	if err != nil {
		t.Fatalf("ny NextFire() error = %v", err)
	}
	if tNext.Equal(nNext) {
		t.Errorf("9am in Tokyo and 9am in New York fired at the same instant %v; the timezone was ignored", tNext)
	}
	// 09:00 JST on 2026-08-10 is 00:00 UTC that day.
	if got := tNext.UTC(); got.Hour() != 0 {
		t.Errorf("tokyo 9am next fire in UTC = %v, want the top of the UTC hour 0", got)
	}

	// A bad timezone is refused at validation, not stored.
	bad := &schedule.Schedule{Cron: "0 9 * * *", Timezone: "Mars/Olympus", Playbook: "p.yml"}
	if err := bad.Validate(); err == nil {
		t.Error("Validate() accepted a nonexistent timezone; it must be refused")
	}

	// No timezone leaves the expression server-local, unchanged from before the field existed.
	local := &schedule.Schedule{Cron: "0 9 * * *", Playbook: "p.yml"}
	if _, err := local.NextFire(after); err != nil {
		t.Errorf("a schedule with no timezone should still fire: %v", err)
	}
}
