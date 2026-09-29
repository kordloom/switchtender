package importer

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestFromCron pins the crontab parser: which lines become schedules, which are skipped with a
// warning, the system user column, and the inventory carried onto each schedule.
func TestFromCron(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		Name          string
		Crontab       string
		Inventory     string
		System        bool
		WantSchedules int
		WantWarnStems []string
	}{{ // Test 0: Ordinary five-field jobs and macros become schedules.
		Name:          "jobs and macros",
		Crontab:       "0 2 * * * /bin/backup\n@daily /bin/cleanup\n*/15 * * * * echo tick\n",
		Inventory:     "prod",
		WantSchedules: 3,
	}, { // Test 1: Comments and blank lines are ignored, not warned.
		Name:          "comments ignored",
		Crontab:       "# nightly\n\n0 3 * * * /bin/x\n",
		Inventory:     "prod",
		WantSchedules: 1,
	}, { // Test 2: @reboot and env assignments are skipped with a warning.
		Name:          "reboot and env skipped",
		Crontab:       "PATH=/usr/bin\n@reboot /bin/warm\n0 1 * * * /bin/y\n",
		Inventory:     "prod",
		WantSchedules: 1,
		WantWarnStems: []string{"environment variable", "@reboot"},
	}, { // Test 3: The system form carries a user column that is warned, and the command follows it.
		Name:          "system user column",
		Crontab:       "0 2 * * * root /bin/backup\n",
		System:        true,
		Inventory:     "prod",
		WantSchedules: 1,
		WantWarnStems: []string{"user \"root\""},
	}, { // Test 4: No inventory warns, since the schedules would target nothing.
		Name:          "no inventory warns",
		Crontab:       "0 2 * * * /bin/backup\n",
		Inventory:     "",
		WantSchedules: 1,
		WantWarnStems: []string{"no --inventory"},
	}, { // Test 5: A malformed schedule expression is skipped, not stored to fire forever.
		Name:          "unparseable skipped",
		Crontab:       "99 2 * * * /bin/backup\n0 2 * * * /bin/ok\n",
		Inventory:     "prod",
		WantSchedules: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			plan, err := FromCron(test.Inventory, test.System)([]byte(test.Crontab), now)
			if err != nil {
				t.Fatalf("FromCron() error = %v", err)
			}
			if len(plan.Schedules) != test.WantSchedules {
				t.Errorf("schedules = %d, want %d", len(plan.Schedules), test.WantSchedules)
			}
			for _, sc := range plan.Schedules {
				if sc.Inventory != test.Inventory {
					t.Errorf("schedule inventory = %q, want %q", sc.Inventory, test.Inventory)
				}
				if len(sc.Steps) != 1 || sc.Steps[0].Tool != "bash" || sc.Steps[0].Command == "" {
					t.Errorf("schedule %q is not a single bash step: %+v", sc.Name, sc.Steps)
				}
				if sc.NextRunAt == nil {
					t.Errorf("schedule %q has no next-run time, so it would never fire", sc.Name)
				}
			}
			warns := strings.Join(plan.Warnings, "\n")
			for _, stem := range test.WantWarnStems {
				if !strings.Contains(warns, stem) {
					t.Errorf("warnings missing %q; got:\n%s", stem, warns)
				}
			}
		})
	}
}
