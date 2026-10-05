package importer

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/schedule"
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

// TestCronTZSetsTheZoneTheSchedulesBelowItAreReadIn covers a whole-hours error that arrived silently.
//
// CRON_TZ is not an ordinary environment variable. Vixie cron reads the schedule of every line below
// it in that zone, so a crontab that sets it and then runs a nightly job at 02:00 means 02:00 there,
// not 02:00 wherever the server happens to sit. It was matched as environment, skipped, and warned
// about as a variable that does not carry, so every job below it imported at the server's local time
// while the report named the wrong loss. A backup window moves by hours that way.
func TestCronTZSetsTheZoneTheSchedulesBelowItAreReadIn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Crontab is the input.
		Crontab string
		// WantZones is the timezone each imported schedule must carry, in order.
		WantZones []string
		// WantWarning is a phrase the report must carry, empty when it must not warn about the zone.
		WantWarning string
	}{{
		Name:      "no CRON_TZ means the server's local time, as the crontab meant",
		Crontab:   "0 2 * * * /usr/bin/backup\n",
		WantZones: []string{schedule.ServerZone()},
	}, {
		Name:      "CRON_TZ applies to the lines below it",
		Crontab:   "CRON_TZ=America/Chicago\n0 2 * * * /usr/bin/backup\n",
		WantZones: []string{"America/Chicago"},
	}, {
		Name: "a later CRON_TZ changes the zone from there down",
		Crontab: "CRON_TZ=America/Chicago\n0 2 * * * /usr/bin/first\n" +
			"CRON_TZ=Europe/Berlin\n0 3 * * * /usr/bin/second\n",
		WantZones: []string{"America/Chicago", "Europe/Berlin"},
	}, {
		Name:      "a quoted zone is read without its quotes",
		Crontab:   "CRON_TZ=\"Asia/Tokyo\"\n0 4 * * * /usr/bin/backup\n",
		WantZones: []string{"Asia/Tokyo"},
	}, {
		Name:        "a zone this system does not know is named as the loss it is",
		Crontab:     "CRON_TZ=Mars/Olympus\n0 2 * * * /usr/bin/backup\n",
		WantZones:   []string{schedule.ServerZone()},
		WantWarning: "not a zone this system knows",
	}, {
		Name:        "an ordinary variable still reports as one",
		Crontab:     "MAILTO=ops@example.com\n0 2 * * * /usr/bin/backup\n",
		WantZones:   []string{schedule.ServerZone()},
		WantWarning: "sets an environment variable",
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			plan, err := FromCron("", false)([]byte(test.Crontab), time.Unix(0, 0).UTC())
			if err != nil {
				t.Fatalf("FromCron() error = %v", err)
			}
			if len(plan.Schedules) != len(test.WantZones) {
				t.Fatalf("imported %d schedule(s), want %d: %v",
					len(plan.Schedules), len(test.WantZones), plan.Warnings)
			}
			for i, want := range test.WantZones {
				if got := plan.Schedules[i].Timezone; got != want {
					t.Errorf("schedule %d reads in %q, want %q. A schedule imported in the wrong zone "+
						"fires whole hours from when its operator set it.", i, got, want)
				}
			}
			warnings := strings.Join(plan.Warnings, "\n")
			if test.WantWarning != "" && !strings.Contains(warnings, test.WantWarning) {
				t.Errorf("no warning mentioned %q: %v", test.WantWarning, plan.Warnings)
			}
			if test.WantWarning == "" && strings.Contains(warnings, "environment variable") {
				t.Errorf("CRON_TZ was reported as an environment variable that does not carry, which "+
					"names the wrong loss: %v", plan.Warnings)
			}
		})
	}
}

// TestVixieDayFieldsThatCombineDifferentlyAreRefused covers a line that came across character for
// character and fired on days it never had.
//
// Cron treats the two day fields as alternatives when both are restricted and as requirements when
// either is not, and what counts as restricted is where the two disagree. Vixie asks whether the text
// begins with a star, so "*/2" is unrestricted and the pair is ANDed; the parser here asks whether the
// field is a star, so "*/2" restricts and the pair is ORed.
//
// "0 0 1 * */2" therefore meant the first of the month only when it fell on an even weekday, and
// imported to every first of the month plus every even weekday. Anyone checking the expression saw the
// same five fields they had written.
func TestVixieDayFieldsThatCombineDifferentlyAreRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Expr is the schedule expression.
		Expr string
		// Diverges is whether the two crons read the pair differently.
		Diverges bool
		// Why says what the case is about.
		Why string
	}{
		{Expr: "0 0 1 * */2", Diverges: true, Why: "a stepped weekday beside a fixed day of month"},
		{Expr: "0 0 */2 * 1", Diverges: true, Why: "the mirror: a stepped day of month beside a fixed weekday"},
		{Expr: "0 0 */2 * */3", Diverges: true, Why: "both stepped, which cron ANDs and this ORs"},
		{Expr: "0 0 1 * *", Diverges: false, Why: "a plain star weekday is unrestricted to both"},
		{Expr: "0 0 * * 1", Diverges: false, Why: "a plain star day of month is unrestricted to both"},
		{Expr: "0 0 1 * 1", Diverges: false, Why: "both restricted, so both crons offer the choice"},
		{Expr: "0 0 * * *", Diverges: false, Why: "neither restricted"},
		{Expr: "@daily", Diverges: false, Why: "a named schedule has no day fields to disagree about"},
	}
	for _, test := range tests {
		t.Run(test.Expr, func(t *testing.T) {
			t.Parallel()
			why, got := vixieDayFieldsDiverge(test.Expr)
			if got != test.Diverges {
				t.Errorf("%q diverges = %v, want %v (%s)", test.Expr, got, test.Diverges, test.Why)
			}
			if got && why == "" {
				t.Error("a refusal with no reason leaves the operator nothing to recreate the line from")
			}
		})
	}

	// End to end: the line is refused rather than imported firing on the wrong days, and the report
	// says which line and why.
	// A good line beside the divergent one, which is the realistic shape and also proves the refusal
	// takes only the line it is about.
	crontab := "0 3 * * * /usr/local/bin/backup\n0 0 1 * */2 /usr/local/bin/rotate\n"
	plan, err := FromCron("", false)([]byte(crontab), time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatalf("FromCron() error = %v", err)
	}
	if len(plan.Schedules) != 1 {
		t.Errorf("imported %d schedule(s), want only the backup: the rotate line fires on days the "+
			"crontab never did", len(plan.Schedules))
	}
	if !strings.Contains(strings.Join(plan.Warnings, "\n"), "combine differently") {
		t.Errorf("the report does not say why the line was refused: %v", plan.Warnings)
	}
}

// TestACrontabInventoryIsReportedAsThePathItIs pins what an imported crontab does with --inventory.
// The guide said a name matching a stored inventory was wired by id. A schedule's own steps carry an
// inventory as a path and nothing else, so it never was, and the report said nothing. It now says the
// name is kept as a path and how to reach a stored inventory instead.
func TestACrontabInventoryIsReportedAsThePathItIs(t *testing.T) {
	t.Parallel()
	plan, err := FromCron("Scalars", false)([]byte("0 2 * * * /usr/local/bin/backup\n"), importNow)
	if err != nil {
		t.Fatalf("FromCron() error = %v", err)
	}
	if len(plan.Schedules) != 1 || plan.Schedules[0].Inventory != "Scalars" {
		t.Fatalf("schedules = %+v, want one carrying the inventory as given", plan.Schedules)
	}
	found := false
	for _, w := range plan.Warnings {
		if strings.Contains(w, `--inventory "Scalars" is kept on each schedule as a path`) &&
			strings.Contains(w, "make it a template") {
			found = true
		}
	}
	if !found {
		t.Errorf("the inventory kept as a path was not reported: %v", plan.Warnings)
	}
}
