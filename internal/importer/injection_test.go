package importer

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/schedule"
)

// TestImportedInventoryCannotWriteItsOwnDirectives checks that a name or variable out of somebody
// else's export cannot add lines to the inventory it is written into.
//
// A line break is the whole attack. A host named "web1\n[all:vars]\nansible_python_interpreter=..."
// does not land as a strange host name: it closes the line and opens a new section. That variable
// points Ansible at an arbitrary interpreter on the executor for every play in the run, and
// ansible_connection=local redirects the whole play onto the executor itself. Neither has a
// command-line counterpart, so an inventory setting them wins, and the content is written to a temp
// file and passed to ansible-playbook as -i verbatim.
func TestImportedInventoryCannotWriteItsOwnDirectives(t *testing.T) {
	t.Parallel()
	const interpreter = "ansible_python_interpreter"
	tests := []struct {
		Name  string
		Hosts []importHost
		Group []importGroup
	}{{ // Test 0: The host name carries the payload.
		Name: "host name",
		Hosts: []importHost{{
			Name: "web1\n[all:vars]\n" + interpreter + "=/tmp/evil\nansible_connection=local",
		}},
	}, { // Test 1: A host variable value carries it.
		Name: "variable value",
		Hosts: []importHost{{
			Name:      "web1",
			Variables: map[string]any{"ansible_user": "bob\n[all:vars]\n" + interpreter + "=/tmp/evil"},
		}},
	}, { // Test 2: A variable name carries it.
		Name: "variable name",
		Hosts: []importHost{{
			Name:      "web1",
			Variables: map[string]any{"x\n[all:vars]\n" + interpreter: "/tmp/evil"},
		}},
	}, { // Test 3: A group name carries it.
		Name:  "group name",
		Group: []importGroup{{Name: "db\n[all:vars]\n" + interpreter + "=/tmp/evil"}},
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			plan := &Plan{}
			got := buildInventoryINI(plan, "prod fleet", test.Hosts, test.Group)
			if strings.Contains(got, interpreter) {
				t.Errorf("the generated inventory sets %s, which points every play at an "+
					"interpreter of the export's choosing on the executor:\n%s", interpreter, got)
			}
			if strings.Contains(got, "ansible_connection=local") {
				t.Errorf("the generated inventory redirects the play onto the executor:\n%s", got)
			}
			// Whatever was dropped is reported, so a person reviewing the plan learns of it.
			if len(plan.Warnings) == 0 {
				t.Error("something was dropped from the inventory with no warning")
			}
			for _, w := range plan.Warnings {
				if strings.Contains(w, "\n") {
					t.Errorf("a warning spans lines, so the report can be dressed up as several "+
						"messages by the thing it reports on: %q", w)
				}
			}
		})
	}
}

// TestImportedInventoryCannotInjectViaWhitespace checks that a space in a name or value out of
// somebody else's export cannot add host variables to the inventory it is written into.
//
// The line does not need a newline to be taken over. Ansible's ini inventory plugin tokenizes a host
// line on whitespace and reads each token after the host name as a key=value host variable, so a host
// named "web1 ansible_python_interpreter=/tmp/evil" lands as host web1 with an interpreter of the
// export's choosing, and a value "x ansible_connection=local" adds ansible_connection=local, which
// redirects the whole play onto the executor. A name carrying the payload is dropped; a value is
// quoted so it survives as one value. Either way the injected setting never becomes a live variable.
func TestImportedInventoryCannotInjectViaWhitespace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Hosts       []importHost
		Group       []importGroup
		WantDropped bool
	}{{ // Test 0: The host name carries a space-separated payload.
		Name:        "host name",
		Hosts:       []importHost{{Name: "web1 ansible_python_interpreter=/tmp/evil"}},
		WantDropped: true,
	}, { // Test 1: A variable name carries it, since the key side tokenizes too.
		Name: "variable name",
		Hosts: []importHost{{
			Name:      "web1",
			Variables: map[string]any{"x ansible_python_interpreter": "/tmp/evil"},
		}},
		WantDropped: true,
	}, { // Test 2: A variable value carries it and is quoted rather than dropped.
		Name: "variable value",
		Hosts: []importHost{{
			Name:      "web1",
			Variables: map[string]any{"ansible_user": "x ansible_connection=local"},
		}},
		WantDropped: false,
	}, { // Test 3: A group name carries it.
		Name: "group name",
		Group: []importGroup{{
			Name:  "db ansible_python_interpreter=/tmp/evil",
			Hosts: []importHost{{Name: "db1"}},
		}},
		WantDropped: true,
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			plan := &Plan{}
			got := buildInventoryINI(plan, "prod fleet", test.Hosts, test.Group)
			for key := range parseINIHostVars(t, got) {
				if key == "ansible_connection" || key == "ansible_python_interpreter" {
					t.Errorf("the generated inventory set %q as a live host variable, so the export "+
						"took over the run:\n%s", key, got)
				}
			}
			if test.WantDropped && len(plan.Warnings) == 0 {
				t.Error("a hostile name was dropped with no warning, so a reviewer never learns of it")
			}
			for _, w := range plan.Warnings {
				if strings.Contains(w, "\n") {
					t.Errorf("a warning spans lines, so the report can be dressed up as several "+
						"messages by the thing it reports on: %q", w)
				}
			}
		})
	}
}

// TestLegitimateSpacedValueSurvivesAsOneValue checks that quoting keeps a real value with a space
// intact rather than dropping or splitting it, so the safety guard does not cost legitimate data.
func TestLegitimateSpacedValueSurvivesAsOneValue(t *testing.T) {
	t.Parallel()
	plan := &Plan{}
	got := buildInventoryINI(plan, "prod", []importHost{
		{Name: "web1", Variables: map[string]any{"description": "two words"}},
	}, nil)

	want := map[string]string{"description": "two words"}
	if diff := cmp.Diff(want, parseINIHostVars(t, got)); diff != "" {
		t.Errorf("a legitimate spaced value was not preserved as one value (-want +got):\n%s\n"+
			"inventory:\n%s", diff, got)
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("a legitimate value produced warnings: %v", plan.Warnings)
	}
}

// parseINIHostVars reads the host variables out of a generated inventory the way Ansible's ini
// inventory plugin does, so a test can assert what a line actually sets rather than matching
// substrings that a quoted value legitimately contains. It returns the variables merged across every
// host line.
func parseINIHostVars(t *testing.T, content string) map[string]string {
	t.Helper()
	vars := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		if line == "" || strings.HasPrefix(line, "[") {
			continue
		}
		tokens := shlexTokens(line)
		for _, tok := range tokens[1:] { // Skip the host name.
			if k, v, ok := strings.Cut(tok, "="); ok {
				vars[k] = v
			}
		}
	}
	return vars
}

// shlexTokens splits a host line the way Ansible's ini plugin does, with shlex in posix mode: it
// breaks on unquoted whitespace, drops the rest of the line at an unquoted '#', and honors single
// quotes as literal runs and double quotes with a backslash escape before a backslash or a double
// quote. It exists so the injection tests measure the real parse rather than the raw text.
func shlexTokens(s string) []string {
	var tokens []string
	var cur strings.Builder
	inToken := false
	for i := 0; i < len(s); {
		switch c := s[i]; c {
		case '#':
			i = len(s)
		case ' ', '\t':
			if inToken {
				tokens = append(tokens, cur.String())
				cur.Reset()
				inToken = false
			}
			i++
		case '\'':
			inToken = true
			for i++; i < len(s) && s[i] != '\''; i++ {
				cur.WriteByte(s[i])
			}
			i++
		case '"':
			inToken = true
			for i++; i < len(s) && s[i] != '"'; {
				if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\') {
					cur.WriteByte(s[i+1])
					i += 2
					continue
				}
				cur.WriteByte(s[i])
				i++
			}
			i++
		default:
			inToken = true
			cur.WriteByte(c)
			i++
		}
	}
	if inToken {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

// TestOrdinaryInventoryStillImports checks the guard did not break the normal case.
func TestOrdinaryInventoryStillImports(t *testing.T) {
	t.Parallel()
	plan := &Plan{}
	got := buildInventoryINI(plan, "prod", []importHost{
		{Name: "web1", Variables: map[string]any{"ansible_user": "deploy", "port": 22}},
		{Name: "web2"},
	}, []importGroup{{Name: "db", Hosts: []importHost{{Name: "db1"}}}})

	for _, want := range []string{"web1 ansible_user=deploy port=22", "web2", "[db]", "db1"} {
		if !strings.Contains(got, want) {
			t.Errorf("inventory is missing %q:\n%s", want, got)
		}
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("an ordinary inventory produced warnings: %v", plan.Warnings)
	}
}

// TestWarningsAreCapped checks that a hostile export cannot make one preview cost an unbounded
// response. Two warnings are emitted per unmapped credential and the document is somebody else's.
func TestWarningsAreCapped(t *testing.T) {
	t.Parallel()
	plan := &Plan{}
	for i := 0; i < maxWarnings*3; i++ {
		plan.warn("warning %d", i)
	}
	if len(plan.Warnings) > maxWarnings+1 {
		t.Errorf("plan holds %d warnings, want the cap to bound it", len(plan.Warnings))
	}
	if plan.Suppressed() == 0 {
		t.Error("warnings were dropped with no count, so a truncated report looks complete")
	}
}

// TestImportedSchedulesAreUsable checks that a schedule out of an export is validated and comes due.
//
// An unvalidated cron string became a stored row the scheduler logged an error over on every tick,
// and a nil first-fire time reads as "not due yet", so an imported schedule was reported as created
// and then never ran. A migrated nightly job that silently does not fire is the worst shape of this:
// nothing looks broken until the work has not happened for a month.
func TestImportedSchedulesAreUsable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	plan := &Plan{}
	plan.addSchedule(&schedule.Schedule{ID: "sch_1", Name: "nightly", Cron: "0 2 * * *", TemplateID: "tpl_1"}, "test", now)
	plan.addSchedule(&schedule.Schedule{ID: "sch_2", Name: "broken", Cron: "*/0 * * * *", TemplateID: "tpl_1"}, "test", now)
	plan.addSchedule(&schedule.Schedule{ID: "sch_3", Name: "nonsense", Cron: "not a cron at all ; rm -rf /", TemplateID: "tpl_1"}, "test", now)

	if len(plan.Schedules) != 1 {
		t.Fatalf("plan holds %d schedules, want only the valid one", len(plan.Schedules))
	}
	got := plan.Schedules[0]
	if got.Name != "nightly" {
		t.Errorf("kept %q, want the valid schedule", got.Name)
	}
	if got.NextRunAt == nil {
		t.Error("the schedule has no first fire time, so the scheduler skips it and it never runs")
	}
	if len(plan.Warnings) != 2 {
		t.Errorf("warnings = %v, want one for each schedule that was refused", plan.Warnings)
	}
}

// TestRRULECannotChooseTheCadence pins that an export cannot decide how often a schedule fires.
//
// The rule's fields were written into a cron expression unvalidated, so a schedule named "Nightly at
// 2am" carrying BYMINUTE=* and BYHOUR=* imported as "* * * * *": a run every minute, enabled, with
// no warning. Combined with a scheduler that reads an impossible expression as always due, an
// import file was a run storm.
func TestRRULECannotChooseTheCadence(t *testing.T) {
	t.Parallel()
	hostile := []string{
		"DTSTART:20260101T020000Z RRULE:FREQ=DAILY;BYMINUTE=*;BYHOUR=*",
		"DTSTART:20260101T020000Z RRULE:FREQ=DAILY;BYMINUTE=0,15,30,45",
		"DTSTART:20260101T020000Z RRULE:FREQ=DAILY;BYHOUR=*/1",
		"DTSTART:20260101T020000Z RRULE:FREQ=MONTHLY;BYMONTHDAY=*",
		"DTSTART:20260101T020000Z RRULE:FREQ=DAILY;BYMINUTE=99",
		"DTSTART:20260101T020000Z RRULE:FREQ=DAILY;BYHOUR=-1",
	}
	for _, rule := range hostile {
		got, ok := RRULEToCron(rule)
		if ok {
			t.Errorf("RRULEToCron(%q) produced %q; the file being imported chose the cadence",
				rule, got)
		}
	}

	// A step that does not divide its range evenly is not the cadence it looks like: "*/45" fires
	// at 0 and 45 past the hour, a 45 minute gap then a 15 minute one.
	uneven := []string{
		"DTSTART:20260101T000000Z RRULE:FREQ=MINUTELY;INTERVAL=45",
		"DTSTART:20260101T000000Z RRULE:FREQ=MINUTELY;INTERVAL=7",
		"DTSTART:20260101T000000Z RRULE:FREQ=HOURLY;INTERVAL=5",
		"DTSTART:20260101T000000Z RRULE:FREQ=HOURLY;INTERVAL=7",
	}
	for _, rule := range uneven {
		if got, ok := RRULEToCron(rule); ok {
			t.Errorf("RRULEToCron(%q) produced %q, which does not fire at that interval", rule, got)
		}
	}

	// Ordinary rules still convert, so the guard did not close the importer.
	fine := map[string]string{
		"DTSTART:20260101T020000Z RRULE:FREQ=DAILY":                      "0 2 * * *",
		"DTSTART:20260101T023000Z RRULE:FREQ=DAILY;BYMINUTE=30;BYHOUR=2": "30 2 * * *",
		"DTSTART:20260101T000000Z RRULE:FREQ=MINUTELY;INTERVAL=15":       "*/15 * * * *",
		"DTSTART:20260101T000000Z RRULE:FREQ=HOURLY;INTERVAL=6":          "0 */6 * * *",
	}
	for rule, want := range fine {
		got, ok := RRULEToCron(rule)
		if !ok {
			t.Errorf("RRULEToCron(%q) refused an ordinary rule", rule)
			continue
		}
		if got != want {
			t.Errorf("RRULEToCron(%q) = %q, want %q", rule, got, want)
		}
	}
}
