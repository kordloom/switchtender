package schedule_test

import (
	"errors"
	"testing"
	"time"

	"github.com/dcadolph/railwarden/internal/schedule"
	"github.com/dcadolph/railwarden/internal/scheduletest"
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
