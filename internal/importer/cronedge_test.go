package importer

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// TestCronAlwaysSaysWhereTheImportedLineRuns pins the warning that is emitted whether or not an
// inventory was named. A crontab line ran on the machine it was taken from; imported it becomes a
// shell step, and a shell step runs where SwitchTender runs. The failure mode is a command running
// in the wrong place and reporting success, so it is said plainly, once.
func TestCronAlwaysSaysWhereTheImportedLineRuns(t *testing.T) {
	t.Parallel()
	for testNum, inventory := range []string{"", "prod"} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			plan, err := FromCron(inventory, false)([]byte("0 2 * * * /bin/backup\n"), importNow)
			if err != nil {
				t.Fatalf("FromCron() error = %v", err)
			}
			if _, ok := warningContaining(t, plan.Warnings,
				"runs on the SwitchTender host"); !ok {
				t.Errorf("the placement warning is missing.\nwarnings: %v", plan.Warnings)
			}
		})
	}
}

// TestCronRefusesACrontabWithNothingSchedulable pins that a file holding no job line is a refusal
// rather than a plan of zeros. An operator who pointed this at the wrong file must not be told the
// import succeeded.
func TestCronRefusesACrontabWithNothingSchedulable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Crontab string
	}{
		{Name: "empty", Crontab: ""},                                        // Test 0.
		{Name: "only comments", Crontab: "# nothing here\n# really\n"},      // Test 1.
		{Name: "only blank lines", Crontab: "\n\n   \n\t\n"},                // Test 2.
		{Name: "only environment", Crontab: "PATH=/usr/bin\nMAILTO=root\n"}, // Test 3.
		{Name: "only reboot", Crontab: "@reboot /bin/warm\n"},               // Test 4.
		{Name: "schedule without a command", Crontab: "0 2 * * *\n"},        // Test 5.
		{Name: "too few fields", Crontab: "0 2 * * /bin/x\n"},               // Test 6: the fifth
		// field is eaten as part of the schedule, leaving no command.
		{Name: "macro without a command", Crontab: "@daily\n"}, // Test 7.
		{Name: "not a crontab", Crontab: "hello world\n"},      // Test 8.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			_, err := FromCron("prod", false)([]byte(test.Crontab), importNow)
			if !errors.Is(err, ErrNothingRecognized) {
				t.Fatalf("FromCron(%q) error = %v, want ErrNothingRecognized", test.Crontab, err)
			}
		})
	}
}

// TestCronSkippedLineIsClippedInTheReport pins that a long unparseable line does not flood the
// report. A crontab is a file somebody else wrote, and a report an operator cannot read is one they
// stop reading.
func TestCronSkippedLineIsClippedInTheReport(t *testing.T) {
	t.Parallel()
	long := "notacron " + strings.Repeat("x", 200)
	crontab := long + "\n0 2 * * * /bin/ok\n"
	plan, err := FromCron("prod", false)([]byte(crontab), importNow)
	if err != nil {
		t.Fatalf("FromCron() error = %v", err)
	}
	warning, ok := warningContaining(t, plan.Warnings, "is not a schedule and was skipped")
	if !ok {
		t.Fatalf("the unparseable line was not reported.\nwarnings: %v", plan.Warnings)
	}
	if strings.Contains(warning, strings.Repeat("x", 100)) {
		t.Errorf("the skipped line was not clipped: %s", warning)
	}
	if !strings.Contains(warning, "...") {
		t.Errorf("the clipped line does not say it was clipped: %s", warning)
	}
}

// TestClipLineTrimsOnlyPastTheLimit pins the boundary of the crontab line clipper, so a line right
// at the limit is shown whole and one past it is marked as shortened.
func TestClipLineTrimsOnlyPastTheLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		In         string
		WantResult string
	}{
		{Name: "short", In: "0 2 * * * /bin/x", WantResult: "0 2 * * * /bin/x"}, // Test 0.
		{Name: "empty", In: "", WantResult: ""},                                 // Test 1.
		{Name: "exactly 60", In: strings.Repeat("a", 60),
			WantResult: strings.Repeat("a", 60)}, // Test 2.
		{Name: "61", In: strings.Repeat("a", 61),
			WantResult: strings.Repeat("a", 60) + "..."}, // Test 3.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := clipLine(test.In); got != test.WantResult {
				t.Errorf("clipLine() = %q, want %q", got, test.WantResult)
			}
		})
	}
}

// TestCronLineTooLongIsAReadFailure pins that a crontab holding a line past the scanner's buffer is
// an error rather than a partial import. An import that silently stopped halfway through a file
// would report a plan the operator would take as the whole crontab.
func TestCronLineTooLongIsAReadFailure(t *testing.T) {
	t.Parallel()
	crontab := "0 2 * * * /bin/ok\n0 3 * * * " + strings.Repeat("x", 2<<20) + "\n"
	_, err := FromCron("prod", false)([]byte(crontab), importNow)
	if err == nil {
		t.Fatal("FromCron() error = nil, want the oversized line to fail the read")
	}
	if !strings.Contains(err.Error(), "read crontab") {
		t.Errorf("error = %v, want it to name the read", err)
	}
}

// TestSplitCronLineFindsTheCommandOrRefuses pins the line splitter, including the system form whose
// user column sits between the schedule and the command. Reading the user as part of the command
// would run "root /bin/backup" as a shell line, which fails in a way nothing explains.
func TestSplitCronLineFindsTheCommandOrRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Raw         string
		System      bool
		WantExpr    string
		WantUser    string
		WantCommand string
		WantOK      bool
	}{
		{Name: "five fields", Raw: "0 2 * * * /bin/backup",
			WantExpr: "0 2 * * *", WantCommand: "/bin/backup", WantOK: true}, // Test 0.
		{Name: "extra spacing", Raw: "0   2  *  *  *    /bin/backup  now",
			WantExpr: "0 2 * * *", WantCommand: "/bin/backup  now", WantOK: true}, // Test 1.
		{Name: "tabs", Raw: "0\t2\t*\t*\t*\t/bin/backup",
			WantExpr: "0 2 * * *", WantCommand: "/bin/backup", WantOK: true}, // Test 2.
		{Name: "macro", Raw: "@daily /bin/cleanup",
			WantExpr: "@daily", WantCommand: "/bin/cleanup", WantOK: true}, // Test 3.
		{Name: "reboot macro", Raw: "@reboot /bin/warm",
			WantExpr: "@reboot", WantCommand: "/bin/warm", WantOK: true}, // Test 4.
		{Name: "system form", Raw: "0 2 * * * root /bin/backup", System: true,
			WantExpr: "0 2 * * *", WantUser: "root", WantCommand: "/bin/backup",
			WantOK: true}, // Test 5.
		{Name: "system macro", Raw: "@daily root /bin/backup", System: true,
			WantExpr: "@daily", WantUser: "root", WantCommand: "/bin/backup",
			WantOK: true}, // Test 6.
		{Name: "command with pipes", Raw: "0 2 * * * /bin/x | tee /var/log/x",
			WantExpr: "0 2 * * *", WantCommand: "/bin/x | tee /var/log/x", WantOK: true}, // Test 7.
		{Name: "no command", Raw: "0 2 * * *", WantOK: false},               // Test 8.
		{Name: "trailing spaces only", Raw: "0 2 * * *    ", WantOK: false}, // Test 9.
		{Name: "four fields", Raw: "0 2 * *", WantOK: false},                // Test 10.
		{Name: "macro alone", Raw: "@daily", WantOK: false},                 // Test 11.
		{Name: "system with no command", Raw: "0 2 * * * root", System: true,
			WantOK: false}, // Test 12.
		{Name: "empty", Raw: "", WantOK: false}, // Test 13.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			expr, user, command, ok := splitCronLine(test.Raw, test.System)
			if ok != test.WantOK {
				t.Fatalf("splitCronLine(%q, %v) ok = %v, want %v",
					test.Raw, test.System, ok, test.WantOK)
			}
			if !ok {
				return
			}
			if expr != test.WantExpr || user != test.WantUser || command != test.WantCommand {
				t.Errorf("splitCronLine(%q, %v) = %q, %q, %q, want %q, %q, %q",
					test.Raw, test.System, expr, user, command,
					test.WantExpr, test.WantUser, test.WantCommand)
			}
		})
	}
}

// TestCutFieldSplitsOnTheFirstRunOfWhitespace pins the small helper the splitter is built on. A
// field boundary read wrong shifts every later field, which turns a schedule into a command.
func TestCutFieldSplitsOnTheFirstRunOfWhitespace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		In        string
		WantField string
		WantRest  string
	}{
		{Name: "two fields", In: "a b", WantField: "a", WantRest: "b"},              // Test 0.
		{Name: "leading space", In: "   a b", WantField: "a", WantRest: "b"},        // Test 1.
		{Name: "tab separated", In: "a\tb", WantField: "a", WantRest: "b"},          // Test 2.
		{Name: "run collapsed", In: "a   \t  b c", WantField: "a", WantRest: "b c"}, // Test 3.
		{Name: "single field", In: "only", WantField: "only", WantRest: ""},         // Test 4.
		{Name: "trailing space", In: "a   ", WantField: "a", WantRest: ""},          // Test 5.
		{Name: "empty", In: "", WantField: "", WantRest: ""},                        // Test 6.
		{Name: "only whitespace", In: "   ", WantField: "", WantRest: ""},           // Test 7.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			field, rest := cutField(test.In)
			if field != test.WantField || rest != test.WantRest {
				t.Errorf("cutField(%q) = %q, %q, want %q, %q",
					test.In, field, rest, test.WantField, test.WantRest)
			}
		})
	}
}

// TestCronEnvironmentAssignmentIsNamedNotRun pins that a crontab environment line is reported rather
// than turned into a schedule. PATH=/usr/bin is not a job, and importing it as one would create a
// schedule running an assignment as a command.
func TestCronEnvironmentAssignmentIsNamedNotRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		Line      string
		WantIsEnv bool
	}{
		{Name: "path", Line: "PATH=/usr/bin:/bin", WantIsEnv: true},                 // Test 0.
		{Name: "mailto", Line: "MAILTO=root", WantIsEnv: true},                      // Test 1.
		{Name: "spaced equals", Line: "SHELL = /bin/sh", WantIsEnv: true},           // Test 2.
		{Name: "leading underscore", Line: "_X=1", WantIsEnv: true},                 // Test 3.
		{Name: "empty value", Line: "MAILTO=", WantIsEnv: true},                     // Test 4.
		{Name: "a schedule is not an assignment", Line: "0 2 * * * FOO=bar /bin/x"}, // Test 5.
		{Name: "a macro is not an assignment", Line: "@daily FOO=bar /bin/x"},       // Test 6.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			crontab := test.Line + "\n0 3 * * * /bin/always\n"
			plan, err := FromCron("prod", false)([]byte(crontab), importNow)
			if err != nil {
				t.Fatalf("FromCron() error = %v", err)
			}
			wantSchedules := 2
			if test.WantIsEnv {
				wantSchedules = 1
			}
			if len(plan.Schedules) != wantSchedules {
				t.Fatalf("schedules = %d, want %d.\nwarnings: %v",
					len(plan.Schedules), wantSchedules, plan.Warnings)
			}
			_, warned := warningContaining(t, plan.Warnings, "sets an environment variable")
			if warned != test.WantIsEnv {
				t.Errorf("environment warning = %v, want %v.\nwarnings: %v",
					warned, test.WantIsEnv, plan.Warnings)
			}
		})
	}
}

// TestCronScheduleCarriesItsCommandAsASingleBashStep pins the shape of what an imported line
// becomes, including the line number in its name so an operator can find the original. A step whose
// command was mangled runs something the crontab did not say.
func TestCronScheduleCarriesItsCommandAsASingleBashStep(t *testing.T) {
	t.Parallel()
	const command = `/usr/bin/backup --to "s3://bucket/path" && echo done # not a comment here`
	crontab := "# header\n\n0 2 * * * " + command + "\n"
	plan, err := FromCron("prod", false)([]byte(crontab), importNow)
	if err != nil {
		t.Fatalf("FromCron() error = %v", err)
	}
	if len(plan.Schedules) != 1 {
		t.Fatalf("schedules = %d, want 1", len(plan.Schedules))
	}
	sc := plan.Schedules[0]
	if sc.Name != "cron line 3" {
		t.Errorf("name = %q, want it to carry the original line number", sc.Name)
	}
	if sc.Cron != "0 2 * * *" {
		t.Errorf("cron = %q, want %q", sc.Cron, "0 2 * * *")
	}
	if sc.Inventory != "prod" {
		t.Errorf("inventory = %q, want %q", sc.Inventory, "prod")
	}
	if !sc.Enabled {
		t.Error("an imported crontab line is not enabled")
	}
	if sc.NextRunAt == nil {
		t.Error("the schedule has no next-run time, so it would never fire")
	}
	wantSteps := []run.PipelineStep{{Name: "cron", Tool: run.ToolBash, Command: command}}
	if diff := cmp.Diff(wantSteps, sc.Steps, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("steps mismatch (-want +got):\n%s", diff)
	}
}

// TestCronUnparseableExpressionIsNeverStored pins that a line whose cadence the scheduler refuses
// does not become a row. An unparseable expression stored anyway makes the scheduler log an error on
// every tick forever, and one that parses but never comes due is read as due on every tick.
func TestCronUnparseableExpressionIsNeverStored(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Line string
	}{
		{Name: "minute out of range", Line: "99 2 * * * /bin/x"}, // Test 0.
		{Name: "hour out of range", Line: "0 99 * * * /bin/x"},   // Test 1.
		{Name: "never comes due", Line: "0 0 30 2 * /bin/x"},     // Test 2.
		{Name: "unknown macro", Line: "@sometimes /bin/x"},       // Test 3.
		{Name: "nonsense fields", Line: "a b c d e /bin/x"},      // Test 4.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			crontab := test.Line + "\n0 3 * * * /bin/ok\n"
			plan, err := FromCron("prod", false)([]byte(crontab), importNow)
			if err != nil {
				t.Fatalf("FromCron() error = %v", err)
			}
			if len(plan.Schedules) != 1 {
				t.Fatalf("schedules = %d, want only the valid line.\nwarnings: %v",
					len(plan.Schedules), plan.Warnings)
			}
			if _, ok := warningContaining(t, plan.Warnings, "was not imported"); !ok {
				if _, skipped := warningContaining(t, plan.Warnings,
					"is not a schedule and was skipped"); !skipped {
					t.Errorf("the refused line was not reported.\nwarnings: %v", plan.Warnings)
				}
			}
		})
	}
}

// TestCronSystemFormNamesTheUserItRanAs pins that the user column is surfaced rather than silently
// run as whoever the server runs as. A backup that ran as root and now runs as the service account
// is a job that fails, or worse, half succeeds.
func TestCronSystemFormNamesTheUserItRanAs(t *testing.T) {
	t.Parallel()
	crontab := "0 2 * * * postgres /usr/bin/pg_dump\n"
	plan, err := FromCron("prod", true)([]byte(crontab), importNow)
	if err != nil {
		t.Fatalf("FromCron() error = %v", err)
	}
	if _, ok := warningContaining(t, plan.Warnings, `ran as user "postgres"`,
		"server's execution account"); !ok {
		t.Errorf("the user column was not reported.\nwarnings: %v", plan.Warnings)
	}
	if got := plan.Schedules[0].Steps[0].Command; got != "/usr/bin/pg_dump" {
		t.Errorf("command = %q, want the user column stripped from it", got)
	}
}

// TestCronUnicodeCommandSurvives pins that a command written in another script is carried verbatim
// into the step. Rewriting somebody's command is the one thing this import must not do.
func TestCronUnicodeCommandSurvives(t *testing.T) {
	t.Parallel()
	const command = "/opt/バックアップ/実行.sh --名前 生产"
	plan, err := FromCron("prod", false)([]byte("0 2 * * * "+command+"\n"), importNow)
	if err != nil {
		t.Fatalf("FromCron() error = %v", err)
	}
	if got := plan.Schedules[0].Steps[0].Command; got != command {
		t.Errorf("command = %q, want %q", got, command)
	}
}
