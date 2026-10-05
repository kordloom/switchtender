package importer

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/template"
)

// rundeckPlan runs one Rundeck YAML document through the importer and fails the test if it does not
// parse, so each case below asserts on the plan rather than on the plumbing.
func rundeckPlan(t *testing.T, inventory, doc string) *Plan {
	t.Helper()
	plan, err := FromRundeck(inventory)([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromRundeck() error = %v\ndocument:\n%s", err, doc)
	}
	return plan
}

// TestRundeckRefusesADocumentItCannotRead pins the parse failures, including the one that once
// blamed the top-level shape for a single quoted field inside a job. An error naming the wrong thing
// sends an operator editing a file that was fine.
func TestRundeckRefusesADocumentItCannotRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Doc  string
	}{
		{Name: "not yaml", Doc: "\t- name: x\n  bad: [\n"},           // Test 0.
		{Name: "scalar root", Doc: "just a string"},                  // Test 1.
		{Name: "mapping without jobs", Doc: "meta:\n  version: 1\n"}, // Test 2.
		{Name: "empty", Doc: ""},                                     // Test 3.
		{Name: "null document", Doc: "null\n"},                       // Test 4.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			plan, err := FromRundeck("prod")([]byte(test.Doc), importNow)
			if err == nil {
				t.Fatalf("FromRundeck() error = nil, want a refusal; plan = %+v", plan)
			}
			if plan != nil {
				t.Errorf("FromRundeck() returned a plan alongside an error")
			}
		})
	}
}

// TestRundeckAcceptsBothExportShapes pins that a bare job list and a list wrapped in a mapping both
// import. Rundeck writes both, and refusing one would send an operator converting a file by hand.
func TestRundeckAcceptsBothExportShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Doc  string
	}{
		{Name: "bare list", Doc: "- name: nightly\n  sequence:\n    commands:\n" +
			"      - exec: /bin/backup\n"}, // Test 0.
		{Name: "wrapped list", Doc: "jobs:\n  - name: nightly\n    sequence:\n      commands:\n" +
			"        - exec: /bin/backup\n"}, // Test 1.
		{Name: "json list", Doc: `[{"name": "nightly",
			"sequence": {"commands": [{"exec": "/bin/backup"}]}}]`}, // Test 2: JSON is valid YAML.
		{Name: "json wrapped", Doc: `{"jobs": [{"name": "nightly",
			"sequence": {"commands": [{"exec": "/bin/backup"}]}}]}`}, // Test 3.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			plan := rundeckPlan(t, "prod", test.Doc)
			if len(plan.Templates) != 1 || plan.Templates[0].Name != "nightly" {
				t.Fatalf("templates = %+v, want one named nightly", plan.Templates)
			}
			if plan.Templates[0].Inventory != "prod" {
				t.Errorf("Inventory = %q, want the one the caller named", plan.Templates[0].Inventory)
			}
		})
	}
}

// TestRundeckWithoutAnInventorySaysSo pins the warning that keeps an operator from discovering at
// launch that every imported template targets nothing. Rundeck dispatches by node filter, so there
// is no inventory to infer and guessing one is the thing an importer must not do.
func TestRundeckWithoutAnInventorySaysSo(t *testing.T) {
	t.Parallel()
	const doc = "- name: nightly\n  sequence:\n    commands:\n      - exec: /bin/backup\n"
	plan := rundeckPlan(t, "", doc)
	if _, ok := warningContaining(t, plan.Warnings, "no inventory was named"); !ok {
		t.Errorf("a missing inventory was not reported.\nwarnings: %v", plan.Warnings)
	}
	withInventory := rundeckPlan(t, "prod", doc)
	if _, ok := warningContaining(t, withInventory.Warnings, "no inventory was named"); ok {
		t.Error("the missing-inventory warning fired even though an inventory was named")
	}
}

// TestRundeckJobNameIsQualifiedByItsGroup pins that two jobs of the same name in different folders
// do not collide. Two templates sharing a name is a migration an operator cannot tell apart.
func TestRundeckJobNameIsQualifiedByItsGroup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Job      rundeckJob
		WantName string
	}{
		{Name: "plain", Job: rundeckJob{Name: "backup"}, WantName: "backup"}, // Test 0.
		{Name: "grouped", Job: rundeckJob{Name: "backup", Group: "ops"},
			WantName: "ops/backup"}, // Test 1.
		{Name: "nested group", Job: rundeckJob{Name: "backup", Group: "ops/nightly"},
			WantName: "ops/nightly/backup"}, // Test 2.
		{Name: "slashes trimmed", Job: rundeckJob{Name: "backup", Group: "/ops/"},
			WantName: "ops/backup"}, // Test 3.
		{Name: "spaced", Job: rundeckJob{Name: "  backup  ", Group: "  ops  "},
			WantName: "ops/backup"}, // Test 4.
		{Name: "group only", Job: rundeckJob{Name: "", Group: "ops"}, WantName: ""},    // Test 5.
		{Name: "blank name", Job: rundeckJob{Name: "   ", Group: "ops"}, WantName: ""}, // Test 6.
		{Name: "unicode", Job: rundeckJob{Name: "バックアップ", Group: "運用"},
			WantName: "運用/バックアップ"}, // Test 7.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := rundeckJobName(test.Job); got != test.WantName {
				t.Errorf("rundeckJobName(%+v) = %q, want %q", test.Job, got, test.WantName)
			}
		})
	}
}

// TestRundeckSkipsAJobWithNoName pins that a nameless job is dropped rather than imported as an
// unnamed template. A template with no name is one nobody can find again.
func TestRundeckSkipsAJobWithNoName(t *testing.T) {
	t.Parallel()
	const doc = "- sequence:\n    commands:\n      - exec: /bin/x\n" +
		"- name: real\n  sequence:\n    commands:\n      - exec: /bin/y\n"
	plan := rundeckPlan(t, "prod", doc)
	if len(plan.Templates) != 1 || plan.Templates[0].Name != "real" {
		t.Fatalf("templates = %+v, want only the named job", plan.Templates)
	}
	if _, ok := warningContaining(t, plan.Warnings, "a job without a name was skipped"); !ok {
		t.Errorf("the nameless job was not reported.\nwarnings: %v", plan.Warnings)
	}
}

// TestRundeckScriptFileStepIsQuotedAndReported pins that a step naming a script on the target runs
// as one argument and is named in the report. A path holding a space or a shell metacharacter that
// went through unquoted would be split, or interpreted, into something else entirely.
func TestRundeckScriptFileStepIsQuotedAndReported(t *testing.T) {
	t.Parallel()
	const doc = `- name: runner
  sequence:
    commands:
      - scriptfile: "/opt/my scripts/run.sh; rm -rf /"
`
	plan := rundeckPlan(t, "prod", doc)
	command := plan.Templates[0].Command
	if !strings.Contains(command, `'/opt/my scripts/run.sh; rm -rf /'`) {
		t.Errorf("script file path was not quoted as one argument:\n%s", command)
	}
	if _, ok := warningContaining(t, plan.Warnings, "runs the script file",
		"must already exist on the target"); !ok {
		t.Errorf("the script file step was not reported.\nwarnings: %v", plan.Warnings)
	}
}

// TestRundeckPluginStepNamesItsTypeWhenTheExportHadOne pins the branch that renders an unmappable
// step's type. A report that says only "a plugin step" leaves the operator guessing which one.
func TestRundeckPluginStepNamesItsTypeWhenTheExportHadOne(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		Step         string
		WantFragment string
	}{
		{Name: "typed plugin", Step: `type: "my.plugin.Step"`,
			WantFragment: `of type "my.plugin.Step"`}, // Test 0.
		{Name: "untyped plugin", Step: `description: mystery`,
			WantFragment: "is a plugin step, which has no equivalent"}, // Test 1.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := "- name: j\n  sequence:\n    commands:\n      - exec: /bin/real\n      - " +
				test.Step + "\n"
			plan := rundeckPlan(t, "prod", doc)
			if _, ok := warningContaining(t, plan.Warnings, test.WantFragment); !ok {
				t.Errorf("missing %q.\nwarnings: %v", test.WantFragment, plan.Warnings)
			}
		})
	}
}

// TestRundeckStepTypeFoldsAndQuotes pins that a plugin type interpolated into a warning is folded to
// one line and quoted, so a step type cannot write extra lines into the report an operator reads.
func TestRundeckStepTypeFoldsAndQuotes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		In         string
		WantResult string
	}{
		{Name: "empty", In: "", WantResult: ""},                              // Test 0.
		{Name: "plain", In: "my.Step", WantResult: ` of type "my.Step"`},     // Test 1.
		{Name: "newline folded", In: "a\nb", WantResult: ` of type "a\\nb"`}, // Test 2.
		{Name: "long clipped", In: strings.Repeat("t", 200),
			WantResult: ` of type "` + strings.Repeat("t", 80) + `..."`}, // Test 3.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := rundeckStepType(test.In); got != test.WantResult {
				t.Errorf("rundeckStepType(%q) = %q, want %q", test.In, got, test.WantResult)
			}
		})
	}
}

// TestRundeckTimeoutReadsBothFormsAndRefusesTheRest pins the timeout conversion. Rundeck writes
// either plain seconds or a duration, and a timeout read wrong is a job that either never stops or
// is killed early, neither of which the operator asked for.
func TestRundeckTimeoutReadsBothFormsAndRefusesTheRest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Timeout     string
		WantSeconds int
		WantWarning string
	}{
		{Name: "absent", Timeout: "", WantSeconds: 0},            // Test 0.
		{Name: "whitespace", Timeout: "   ", WantSeconds: 0},     // Test 1.
		{Name: "bare seconds", Timeout: "600", WantSeconds: 600}, // Test 2.
		{Name: "zero", Timeout: "0", WantSeconds: 0},             // Test 3.
		{Name: "negative", Timeout: "-30", WantSeconds: 0,
			WantWarning: "negative timeout"}, // Test 4.
		{Name: "hours", Timeout: "1h", WantSeconds: 3600},                 // Test 5.
		{Name: "minutes", Timeout: "30m", WantSeconds: 1800},              // Test 6.
		{Name: "compound", Timeout: "1h30m", WantSeconds: 5400},           // Test 7.
		{Name: "sub second truncates", Timeout: "1500ms", WantSeconds: 1}, // Test 8.
		{Name: "negative duration", Timeout: "-1h", WantSeconds: 0,
			WantWarning: "could not be read"}, // Test 9.
		{Name: "days are not a duration", Timeout: "1d", WantSeconds: 0,
			WantWarning: "could not be read"}, // Test 10.
		{Name: "nonsense", Timeout: "soon", WantSeconds: 0,
			WantWarning: "could not be read"}, // Test 11.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := fmt.Sprintf("- name: j\n  timeout: %q\n  sequence:\n    commands:\n"+
				"      - exec: /bin/x\n", test.Timeout)
			plan := rundeckPlan(t, "prod", doc)
			if got := plan.Templates[0].Timeout; got != test.WantSeconds {
				t.Errorf("timeout %q gave %d seconds, want %d", test.Timeout, got, test.WantSeconds)
			}
			if test.WantWarning == "" {
				return
			}
			if _, ok := warningContaining(t, plan.Warnings, test.WantWarning); !ok {
				t.Errorf("missing %q.\nwarnings: %v", test.WantWarning, plan.Warnings)
			}
		})
	}
}

// TestRundeckThreadCountIsReadAsForks pins the quoted-scalar decoder and the field it lands in.
// Rundeck's own published exports quote threadcount, and decoding straight into an int once failed
// the whole document on that one field.
func TestRundeckThreadCountIsReadAsForks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		Value     string
		WantForks int
	}{
		{Name: "bare number", Value: "8", WantForks: 8},     // Test 0.
		{Name: "quoted number", Value: `"8"`, WantForks: 8}, // Test 1.
		{Name: "zero", Value: "0", WantForks: 0},            // Test 2.
		{Name: "empty string", Value: `""`, WantForks: 0},   // Test 3.
		{Name: "spaced", Value: `" 8 "`, WantForks: 8},      // Test 4.
		{Name: "negative", Value: "-4", WantForks: -4},      // Test 5.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := fmt.Sprintf("- name: j\n  nodefilters:\n    dispatch:\n      threadcount: %s\n"+
				"  sequence:\n    commands:\n      - exec: /bin/x\n", test.Value)
			plan := rundeckPlan(t, "prod", doc)
			if got := plan.Templates[0].Forks; got != test.WantForks {
				t.Errorf("threadcount %s gave Forks = %d, want %d",
					test.Value, got, test.WantForks)
			}
		})
	}
}

// TestRundeckDisabledJobAndScheduleBothComeAcrossSwitchedOff pins one policy over four importers.
//
// A disabled schedule used to be left behind here while AWX, Jenkins and Semaphore all carried theirs
// switched off, so the same situation had two answers depending on which tool somebody was leaving.
// Dropping it is the safe half of the problem and loses the other half: the expression is gone, so
// turning the job back on months later means reconstructing the cadence from memory.
//
// What must hold in every format is the direction that does something. Off at the source, off here.
func TestRundeckDisabledJobAndScheduleBothComeAcrossSwitchedOff(t *testing.T) {
	t.Parallel()
	const doc = `- name: disabled-job
  executionEnabled: false
  sequence:
    commands:
      - exec: /bin/x
- name: disabled-schedule
  scheduleEnabled: false
  schedule:
    crontab: "0 0 2 * * ?"
  sequence:
    commands:
      - exec: /bin/y
- name: disabled-job-on-a-schedule
  executionEnabled: false
  schedule:
    crontab: "0 0 3 * * ?"
  sequence:
    commands:
      - exec: /bin/z
`
	plan := rundeckPlan(t, "prod", doc)
	if len(plan.Templates) != 3 {
		t.Fatalf("templates = %d, want 3: every job still imports", len(plan.Templates))
	}
	if len(plan.Schedules) != 2 {
		t.Fatalf("schedules = %d, want both carried rather than dropped: %v",
			len(plan.Schedules), plan.Warnings)
	}
	// The third job is the one that went wrong. Its schedule was enabled in Rundeck and the job
	// itself was not, so Rundeck never ran it, and it imported armed: a parked purge job fired on
	// its old cadence the night after somebody migrated.
	for _, sc := range plan.Schedules {
		if sc.Enabled {
			t.Errorf("schedule %q imported armed, which starts a job the estate had switched off",
				sc.Name)
		}
		if sc.Cron == "" {
			t.Errorf("schedule %q came across with no expression, so carrying it gained nothing",
				sc.Name)
		}
	}
	if _, ok := warningContaining(t, plan.Warnings, `job "disabled-job" is disabled`); !ok {
		t.Errorf("the disabled job was not noted.\nwarnings: %v", plan.Warnings)
	}
	if _, ok := warningContaining(t, plan.Warnings, "switched off at the source"); !ok {
		t.Errorf("nothing said the schedule arrives switched off, so the count of schedules reads as "+
			"a count of cadences that fire.\nwarnings: %v", plan.Warnings)
	}
}

// TestRundeckNodeFilterIsReportedNotTranslated pins that a job selecting hosts by attribute says so.
// SwitchTender targets an inventory, and inventing one from a filter expression would be guessing
// which machines somebody's job runs against.
func TestRundeckNodeFilterIsReportedNotTranslated(t *testing.T) {
	t.Parallel()
	const doc = "- name: j\n  nodefilters:\n    filter: \"tags: web+ os: linux\"\n" +
		"  sequence:\n    commands:\n      - exec: /bin/x\n"
	plan := rundeckPlan(t, "prod", doc)
	if _, ok := warningContaining(t, plan.Warnings, "dispatches by the node filter",
		"tags: web+ os: linux"); !ok {
		t.Errorf("the node filter was not reported.\nwarnings: %v", plan.Warnings)
	}
}

// TestRundeckStructuredScheduleFields pins the field-by-field cadence form and the two parts of it a
// cron expression cannot hold. A seconds field or a year limit dropped without a word changes when
// the job fires.
func TestRundeckStructuredScheduleFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		Schedule     string
		WantCron     string
		WantRRule    string
		WantWarning  string
		WantImported bool
	}{
		{Name: "hour and minute", Schedule: "    time:\n      hour: '2'\n      minute: '30'\n",
			WantCron: "30 2 * * *", WantImported: true}, // Test 0.
		{Name: "unset fields become stars", Schedule: "    time:\n      hour: '*'\n",
			WantCron: "* * * * *", WantImported: true}, // Test 1.
		{Name: "question marks become stars",
			Schedule: "    time:\n      hour: '3'\n      minute: '0'\n" +
				"    dayofmonth:\n      day: '?'\n    weekday:\n      day: '?'\n",
			WantCron: "0 3 * * *", WantImported: true}, // Test 2.
		{Name: "seconds are reported",
			Schedule: "    time:\n      hour: '2'\n      minute: '0'\n      seconds: '30'\n",
			WantCron: "0 2 * * *", WantImported: true,
			WantWarning: "fires at second"}, // Test 3.
		{Name: "zero seconds are not reported",
			Schedule: "    time:\n      hour: '2'\n      minute: '0'\n      seconds: '0'\n",
			WantCron: "0 2 * * *", WantImported: true}, // Test 4.
		{Name: "year is reported",
			Schedule: "    time:\n      hour: '2'\n      minute: '0'\n    year: '2027'\n",
			WantCron: "0 2 * * *", WantImported: true,
			WantWarning: "limited to the year"}, // Test 5.
		{Name: "star year is not reported",
			Schedule: "    time:\n      hour: '2'\n      minute: '0'\n    year: '*'\n",
			WantCron: "0 2 * * *", WantImported: true}, // Test 6.
		{Name: "weekday renumbered",
			Schedule: "    time:\n      hour: '2'\n      minute: '0'\n" +
				"    weekday:\n      day: '2'\n",
			WantCron: "0 2 * * 1", WantImported: true}, // Test 7: Quartz Monday is two.
		{Name: "last weekday comes across as a recurrence",
			Schedule: "    time:\n      hour: '2'\n      minute: '0'\n" +
				"    weekday:\n      day: '6L'\n",
			WantImported: true,
			WantRRule:    "RRULE:FREQ=MONTHLY;BYDAY=-1FR;BYHOUR=2;BYMINUTE=0;BYSECOND=0"}, // Test 8.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := "- name: j\n  schedule:\n" + test.Schedule +
				"  sequence:\n    commands:\n      - exec: /bin/x\n"
			plan := rundeckPlan(t, "prod", doc)
			if test.WantImported {
				if len(plan.Schedules) != 1 {
					t.Fatalf("schedules = %d, want 1.\nwarnings: %v",
						len(plan.Schedules), plan.Warnings)
				}
				if got := plan.Schedules[0].Cron; got != test.WantCron {
					t.Errorf("cron = %q, want %q", got, test.WantCron)
				}
				if got := plan.Schedules[0].RRule; !strings.HasSuffix(got, test.WantRRule) ||
					(test.WantRRule == "") != (got == "") {
					t.Errorf("rrule = %q, want it to end %q", got, test.WantRRule)
				}
			} else if len(plan.Schedules) != 0 {
				t.Errorf("schedules = %d, want 0", len(plan.Schedules))
			}
			if test.WantWarning == "" {
				return
			}
			if _, ok := warningContaining(t, plan.Warnings, test.WantWarning); !ok {
				t.Errorf("missing %q.\nwarnings: %v", test.WantWarning, plan.Warnings)
			}
		})
	}
}

// TestQuartzToCronDropsTheExtraFieldsAndRenumbersTheWeekday pins the crontab form. Quartz leads with
// seconds and may end with a year, and it numbers weekdays from one for Sunday. Dropping the extra
// fields without renumbering would shift every weekly job by a day.
func TestQuartzToCronDropsTheExtraFieldsAndRenumbersTheWeekday(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Crontab  string
		WantCron string
		WantOK   bool
	}{
		{Name: "six fields", Crontab: "0 30 2 * * ?",
			WantCron: "30 2 * * *", WantOK: true}, // Test 0.
		{Name: "seven fields", Crontab: "0 30 2 * * ? *",
			WantCron: "30 2 * * *", WantOK: true}, // Test 1.
		{Name: "weekday renumbered", Crontab: "0 0 3 ? * 2",
			WantCron: "0 3 * * 1", WantOK: true}, // Test 2.
		{Name: "weekday range", Crontab: "0 0 3 ? * 2-6",
			WantCron: "0 3 * * 1-5", WantOK: true}, // Test 3.
		{Name: "weekday list", Crontab: "0 0 3 ? * 1,7",
			WantCron: "0 3 * * 0,6", WantOK: true}, // Test 4.
		{Name: "day names pass through", Crontab: "0 0 3 ? * MON-FRI",
			WantCron: "0 3 * * MON-FRI", WantOK: true}, // Test 5.
		{Name: "wednesday keeps its name", Crontab: "0 0 3 ? * WED",
			WantCron: "0 3 * * WED", WantOK: true}, // Test 6: the W is not the nearest-weekday form.
		{Name: "five fields pass through", Crontab: "30 2 * * *",
			WantCron: "30 2 * * *", WantOK: true}, // Test 7.
		{Name: "four fields refused", Crontab: "30 2 * *", WantOK: false},         // Test 8.
		{Name: "eight fields refused", Crontab: "0 0 3 ? * 1 * *", WantOK: false}, // Test 9.
		{Name: "nth weekday refused", Crontab: "0 0 3 ? * 6#3", WantOK: false},    // Test 10.
		{Name: "last weekday refused", Crontab: "0 0 3 ? * 6L", WantOK: false},    // Test 11.
		{Name: "weekday zero refused", Crontab: "0 0 3 ? * 0", WantOK: false},     // Test 12: outside
		// the Quartz range of one to seven, so the schedule is dropped rather than shifted.
		{Name: "weekday eight refused", Crontab: "0 0 3 ? * 8", WantOK: false}, // Test 13.
		{Name: "weekday in a range out of bounds", Crontab: "0 0 3 ? * 1-9",
			WantOK: false}, // Test 14.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			p := &Plan{}
			got, ok := p.quartzToCron(test.Crontab, "j")
			if ok != test.WantOK {
				t.Fatalf("quartzToCron(%q) ok = %v, want %v (got %q, warnings %v)",
					test.Crontab, ok, test.WantOK, got, p.Warnings)
			}
			if ok && got != test.WantCron {
				t.Errorf("quartzToCron(%q) = %q, want %q", test.Crontab, got, test.WantCron)
			}
			if !ok && len(p.Warnings) == 0 {
				t.Errorf("quartzToCron(%q) refused without reporting why", test.Crontab)
			}
		})
	}
}

// TestQuartzSecondsAndYearAreReportedFromACrontab pins that the two fields a cron expression cannot
// hold are named rather than dropped in silence. A job that fired at second thirty now fires at the
// top of the minute, which the operator should hear about once.
func TestQuartzSecondsAndYearAreReportedFromACrontab(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name          string
		Crontab       string
		WantWarning   string
		WantNoWarning bool
	}{
		{Name: "non zero seconds", Crontab: "30 0 2 * * ?",
			WantWarning: "fires at second"}, // Test 0.
		{Name: "zero seconds are quiet", Crontab: "0 0 2 * * ?",
			WantNoWarning: true}, // Test 1.
		{Name: "year limit", Crontab: "0 0 2 * * ? 2027",
			WantWarning: "limited to the year"}, // Test 2.
		{Name: "star year is quiet", Crontab: "0 0 2 * * ? *",
			WantNoWarning: true}, // Test 3.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			p := &Plan{}
			if _, ok := p.quartzToCron(test.Crontab, "j"); !ok {
				t.Fatalf("quartzToCron(%q) refused: %v", test.Crontab, p.Warnings)
			}
			if test.WantNoWarning {
				if len(p.Warnings) != 0 {
					t.Errorf("warnings = %v, want none", p.Warnings)
				}
				return
			}
			if _, ok := warningContaining(t, p.Warnings, test.WantWarning); !ok {
				t.Errorf("missing %q.\nwarnings: %v", test.WantWarning, p.Warnings)
			}
		})
	}
}

// TestRundeckSurveyOptionShapes pins how a job's prompted inputs become survey fields, including the
// two shapes that are reported rather than translated. An option with suggested values is not a
// choice, and importing it as one would refuse an answer Rundeck accepted.
func TestRundeckSurveyOptionShapes(t *testing.T) {
	t.Parallel()
	const doc = `- name: j
  options:
    - name: env
      description: Which environment
      required: true
      enforced: true
      value: prod
      values: [prod, stage]
    - name: hint
      values: [a, b]
    - name: many
      multivalued: true
    - name: plain
    - description: nameless
  sequence:
    commands:
      - exec: /bin/x
`
	plan := rundeckPlan(t, "prod", doc)
	survey := plan.Templates[0].Survey
	if len(survey) != 4 {
		t.Fatalf("survey fields = %d, want 4: the nameless option is dropped", len(survey))
	}
	if survey[0].Type != "choice" || len(survey[0].Choices) != 2 || survey[0].Default != "prod" {
		t.Errorf("enforced option = %+v, want a choice with its default", survey[0])
	}
	if !survey[0].Required || survey[0].Help != "Which environment" {
		t.Errorf("enforced option lost its required flag or description: %+v", survey[0])
	}
	if survey[1].Type != "text" {
		t.Errorf("unenforced option = %q, want free text", survey[1].Type)
	}
	for _, want := range []string{"has an option with no name", "suggests values without enforcing",
		"accepted several values at once"} {
		if _, ok := warningContaining(t, plan.Warnings, want); !ok {
			t.Errorf("missing %q.\nwarnings: %v", want, plan.Warnings)
		}
	}
}

// TestRundeckSecureOptionIsNeverDowngradedToPlainText pins the mapping. Rundeck stores a secure
// option obscured, so it imports as a secret field, never as text. Its default comes from Rundeck's
// key storage, which an export does not carry, so it is left behind, named, and never repeated.
func TestRundeckSecureOptionIsNeverDowngradedToPlainText(t *testing.T) {
	t.Parallel()
	const doc = `- name: j
  options:
    - name: token
      secure: true
      value: s3cret-default
    - name: env
  sequence:
    commands:
      - exec: /bin/x
`
	plan := rundeckPlan(t, "prod", doc)
	want := []template.SurveyField{
		{Var: "token", Label: "token", Type: template.FieldSecret},
		{Var: "env", Label: "env", Type: template.FieldText},
	}
	if diff := cmp.Diff(want, plan.Templates[0].Survey, cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("survey mismatch (-want +got):\n%s", diff)
	}
	if _, ok := warningContaining(t, plan.Warnings, `secret field "token" arrives without its default`); !ok {
		t.Errorf("the default left behind was not named.\nwarnings: %v", plan.Warnings)
	}
	for _, w := range plan.Warnings {
		if strings.Contains(w, "s3cret-default") {
			t.Errorf("the secure option's default leaked into the report: %s", w)
		}
	}
}

// TestRundeckJobMadeOnlyOfUnmappableStepsIsSkipped pins that a template which would report success
// without running anything is never created. An empty template that goes green is worse than a job
// that was not imported.
func TestRundeckJobMadeOnlyOfUnmappableStepsIsSkipped(t *testing.T) {
	t.Parallel()
	const doc = `- name: only-refs
  sequence:
    commands:
      - jobref:
          name: other
      - scripturl: https://example.com/x.sh
      - type: my.plugin
- name: has-work
  sequence:
    commands:
      - exec: /bin/real
`
	plan := rundeckPlan(t, "prod", doc)
	if len(plan.Templates) != 1 || plan.Templates[0].Name != "has-work" {
		t.Fatalf("templates = %+v, want only the job with runnable steps", plan.Templates)
	}
	if _, ok := warningContaining(t, plan.Warnings, `job "only-refs" was skipped`,
		"reported success without running anything"); !ok {
		t.Errorf("the empty job was not reported.\nwarnings: %v", plan.Warnings)
	}
	if _, ok := warningContaining(t, plan.Warnings, "Fetching and running remote code"); !ok {
		t.Errorf("the remote script step was not reported.\nwarnings: %v", plan.Warnings)
	}
}

// TestRundeckKeepGoingDecidesWhetherTheScriptSetsE pins that the imported script stops where Rundeck
// stopped. A job that kept going past a failed step, imported under set -e, would stop at the first
// failure and leave the rest of its work undone.
func TestRundeckKeepGoingDecidesWhetherTheScriptSetsE(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		KeepGoing string
		WantSetE  bool
	}{
		{Name: "default stops", KeepGoing: "", WantSetE: true},       // Test 0.
		{Name: "explicit false", KeepGoing: "false", WantSetE: true}, // Test 1.
		{Name: "keeps going", KeepGoing: "true", WantSetE: false},    // Test 2.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			keep := ""
			if test.KeepGoing != "" {
				keep = "    keepgoing: " + test.KeepGoing + "\n"
			}
			doc := "- name: j\n  sequence:\n" + keep + "    commands:\n      - exec: /bin/x\n"
			plan := rundeckPlan(t, "prod", doc)
			command := plan.Templates[0].Command
			if got := strings.HasPrefix(command, "set -e\n"); got != test.WantSetE {
				t.Errorf("script starts with set -e = %v, want %v.\nscript:\n%s",
					got, test.WantSetE, command)
			}
		})
	}
}

// TestRundeckStepDescriptionCannotWriteItsOwnScriptLines pins that a step description becomes one
// comment line. A description carrying a newline would otherwise write lines of its own into a
// script the operator is about to run.
func TestRundeckStepDescriptionCannotWriteItsOwnScriptLines(t *testing.T) {
	t.Parallel()
	const doc = "- name: j\n  sequence:\n    commands:\n" +
		"      - description: \"first\\nrm -rf /\"\n        exec: /bin/x\n"
	plan := rundeckPlan(t, "prod", doc)
	command := plan.Templates[0].Command
	for _, line := range strings.Split(command, "\n") {
		if strings.TrimSpace(line) == "rm -rf /" {
			t.Fatalf("a step description wrote its own script line:\n%s", command)
		}
	}
	if !strings.Contains(command, `# first\nrm -rf /`) {
		t.Errorf("the description was not folded onto one comment line:\n%s", command)
	}
}

// TestARundeckJobSaysWhatItsStepsLose pins the Rundeck settings a template has no place for. A script
// whose first line runs it with Python was inlined into Bash and failed on its first import line, and
// a step's script arguments, its error handler, the job's retries, its notifications, and an option's
// regex were all dropped without a word. The script is left out and named, the regex becomes the
// survey pattern it is, and every other loss is reported as left out.
func TestARundeckJobSaysWhatItsStepsLose(t *testing.T) {
	t.Parallel()
	const export = `
- name: Nightly report
  retry: '3'
  notification:
    onfailure:
      email: {recipients: ops@example.test}
  options:
    - name: ticket
      regex: 'CHG[0-9]{7}'
  sequence:
    commands:
      - exec: echo start
      - script: |
          #!/usr/bin/env python3
          import sys
          print(sys.argv)
      - script: |
          #!/bin/bash -e
          echo "$1"
        args: 'weekly'
        errorhandler:
          exec: echo cleanup
`
	plan, err := FromRundeck("hosts")([]byte(export), testNow)
	if err != nil {
		t.Fatalf("FromRundeck() error = %v", err)
	}
	if len(plan.Templates) != 1 {
		t.Fatalf("templates = %d, want 1", len(plan.Templates))
	}
	tpl := plan.Templates[0]
	if strings.Contains(tpl.Command, "import sys") {
		t.Errorf("the Python script was inlined into the Bash template:\n%s", tpl.Command)
	}
	if !strings.Contains(tpl.Command, `echo "$1"`) {
		t.Errorf("the Bash script with a flag on its shebang was left out:\n%s", tpl.Command)
	}
	if len(tpl.Survey) != 1 || tpl.Survey[0].Pattern != "CHG[0-9]{7}" {
		t.Errorf("survey = %+v, want the option's regex as the field's pattern", tpl.Survey)
	}
	leftOut := strings.Join(plan.Report().LeftOut, "\n")
	for _, want := range []string{`first line runs it with "python3"`, `passes "weekly" to its script`,
		"has an error handler", "retries up to 3 times", "sends notifications"} {
		if !strings.Contains(leftOut, want) {
			t.Errorf("the summary does not count %q as left out:\n%s", want, leftOut)
		}
	}
}

// TestARundeckArchiveJobCarriesTheSameLosses pins that a job read from a project archive's XML
// reports the same losses as one from a job export, since both feed one mapping.
func TestARundeckArchiveJobCarriesTheSameLosses(t *testing.T) {
	t.Parallel()
	const doc = `<joblist><job><name>Nightly</name><retry delay="10s">2</retry>
  <notification><onfailure><email recipients="ops@example.test"/></onfailure></notification>
  <context><options><option name="ticket" regex="CHG[0-9]{7}"/></options></context>
  <sequence><command><script>echo "$1"</script><scriptargs>weekly</scriptargs>
    <errorhandler><exec>echo cleanup</exec></errorhandler></command></sequence></job></joblist>`
	jobs, err := decodeRundeckXML([]byte(doc))
	if err != nil || len(jobs) != 1 {
		t.Fatalf("decodeRundeckXML() = %d jobs, %v", len(jobs), err)
	}
	job := jobs[0]
	if rundeckRetries(job.Retry) != "2" || job.Notification == nil || job.Options[0].Regex != "CHG[0-9]{7}" {
		t.Errorf("job = retry %v notification %v regex %q, want each read", job.Retry, job.Notification,
			job.Options[0].Regex)
	}
	if cmd := job.Sequence.Commands[0]; cmd.Args != "weekly" || cmd.ErrorHandler == nil {
		t.Errorf("step = args %q error handler %v, want both read", cmd.Args, cmd.ErrorHandler)
	}
}
