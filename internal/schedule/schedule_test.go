package schedule_test

import (
	"errors"
	"fmt"
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

// TestABadTimezoneIsReportedAsATimezone covers two different mistakes that were reported as one.
//
// The zone validator wrapped schedule.ErrBadCron, so every caller rendered a typo in the timezone field as
// "invalid cron expression". An operator who typed America/New_york was told their perfectly good
// "0 2 * * *" was wrong, and had no reason to look at the field that actually was. The preview
// endpoint had the same problem by a different route: it calls NextFire directly, without Validate.
func TestABadTimezoneIsReportedAsATimezone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which mistake is being made.
		Name string
		// Cron and Timezone are the schedule as submitted.
		Cron     string
		Timezone string
		// Want is the sentinel the failure must carry, nil when the schedule is valid.
		Want error
	}{{ // Test 0: A good expression with a zone this system does not know is a timezone fault.
		// Deliberately not a case variant of a real zone: tzdata resolves those on some systems and
		// not others, so asserting on one would pin the platform rather than the behavior.
		Name: "unknown zone", Cron: "0 2 * * *", Timezone: "Nowhere/Nothing",
		Want: schedule.ErrBadTimezone,
	}, { // Test 1: Something that is not a zone name at all.
		Name: "not a zone", Cron: "0 2 * * *", Timezone: "EST=5", Want: schedule.ErrBadTimezone,
	}, { // Test 2: A bad expression is still a cron fault.
		Name: "bad expression", Cron: "not a cron", Timezone: "America/New_York", Want: schedule.ErrBadCron,
	}, { // Test 3: Both correct.
		Name: "both good", Cron: "0 2 * * *", Timezone: "America/New_York", Want: nil,
	}, { // Test 4: No zone at all is the server's own, which is valid.
		Name: "no zone", Cron: "0 2 * * *", Timezone: "", Want: nil,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			// NextFire is the path the preview endpoint takes, which never calls Validate.
			sc := &schedule.Schedule{Cron: test.Cron, Timezone: test.Timezone}
			_, err := sc.NextFire(time.Now())
			if test.Want == nil {
				if err != nil {
					t.Fatalf("%s: NextFire() error = %v, want none", test.Name, err)
				}
				return
			}
			if !errors.Is(err, test.Want) {
				t.Errorf("%s: NextFire() error = %v, want it to carry %v so the message sends the "+
					"operator to the field that is actually wrong", test.Name, err, test.Want)
			}
		})
	}
}
