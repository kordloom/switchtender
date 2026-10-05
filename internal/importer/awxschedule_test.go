package importer

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/template"
)

// TestAWXScheduleOverridesComeAcross pins what an AWX schedule carries beyond its cadence.
//
// A schedule there launches with its own survey answers and extra variables, and with any limit,
// tags, or check mode it was saved with. Every one was dropped, so a nightly check against the canary
// host imported as a real run against the template's whole inventory, with none of the answers the
// playbook expected. A schedule that overrides anything now fires a copy of its template with the
// overrides applied, and the template people launch by hand is left as it was.
func TestAWXScheduleOverridesComeAcross(t *testing.T) {
	t.Parallel()
	const export = `{
	  "projects": [{"name": "web", "scm_type": "git", "scm_url": "https://example.com/web.git"}],
	  "inventory": [{"name": "prod", "hosts": [{"name": "web1"}]}],
	  "job_templates": [{"name": "Deploy", "playbook": "site.yml", "project": "web",
	    "inventory": "prod", "extra_vars": "channel: stable\nrelease: '1.0'",
	    "related": {"schedules": [
	      {"name": "nightly canary", "rrule": "DTSTART:20260101T020000Z RRULE:FREQ=DAILY;INTERVAL=1",
	       "extra_data": {"release": "1.2"}, "limit": "canary", "job_type": "check"},
	      {"name": "plain", "rrule": "DTSTART:20260101T030000Z RRULE:FREQ=DAILY;INTERVAL=1",
	       "extra_data": {}, "limit": "", "job_type": null, "diff_mode": null},
	      {"name": "elsewhere", "rrule": "DTSTART:20260101T040000Z RRULE:FREQ=DAILY;INTERVAL=1",
	       "inventory": "missing"}
	    ]}}]}`
	plan, err := FromAWX([]byte(export), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	byName := map[string]*template.Template{}
	for _, tpl := range plan.Templates {
		byName[tpl.Name] = tpl
	}
	orig, canary := byName["Deploy"], byName["Deploy (nightly canary)"]
	if orig == nil || canary == nil {
		t.Fatalf("templates = %v, want the original and a copy for the schedule that overrides it",
			plan.Templates)
	}
	want := template.Template{
		Name: "Deploy (nightly canary)", Playbook: "site.yml", Limit: "canary", DryRun: true,
		ExtraVars: map[string]any{"channel": "stable", "release": "1.2"},
	}
	if diff := cmp.Diff(want, *canary, cmpopts.IgnoreFields(template.Template{}, "ID", "ProjectID",
		"InventoryID", "CreatedAt"), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the schedule's copy mismatch (-want +got):\n%s", diff)
	}
	if canary.ProjectID != orig.ProjectID || canary.InventoryID != orig.InventoryID {
		t.Errorf("the copy lost the template's project or inventory: %+v", canary)
	}
	if orig.Limit != "" || orig.DryRun || orig.ExtraVars["release"] != "1.0" {
		t.Errorf("the schedule's overrides reached the template people launch by hand: %+v", orig)
	}
	fires := map[string]string{}
	armed := map[string]bool{}
	for _, sc := range plan.Schedules {
		for _, tpl := range plan.Templates {
			if tpl.ID == sc.TemplateID {
				fires[sc.Name] = tpl.Name
			}
		}
		armed[sc.Name] = sc.Enabled
	}
	wantFires := map[string]string{
		"nightly canary": "Deploy (nightly canary)", "plain": "Deploy", "elsewhere": "Deploy (elsewhere)",
	}
	if diff := cmp.Diff(wantFires, fires); diff != "" {
		t.Errorf("which template each schedule fires mismatch (-want +got):\n%s", diff)
	}
	if armed["elsewhere"] {
		t.Error("a schedule whose inventory did not come across imported armed, so it would fire " +
			"against the template's own inventory instead of the one it named")
	}
	if !armed["nightly canary"] || !armed["plain"] {
		t.Errorf("schedules that came across whole were switched off: %v", armed)
	}
	if _, ok := warningContaining(t, plan.Warnings, `schedule "nightly canary"`, "limit canary",
		"job type check", `"Deploy (nightly canary)"`); !ok {
		t.Errorf("the copy and what it carries were not reported.\nwarnings: %v", plan.Warnings)
	}
}

// TestAWXScheduleSurveyAnswersBecomeTheCopysDefaults pins how a schedule's survey answers come across.
//
// A schedule fires with each survey question's default and refuses one that is required with none,
// because nobody is present to answer it. The schedule's answers used to come across as plain extra
// vars on its copy, so every required question on the copy had no default and the schedule refused
// every fire that AWX ran without complaint, and a secret answer came across as the placeholder an
// export holds in its place, which a play would have run with as the password. Each answer is now
// its question's default on the copy, and a secret one, which cannot come across, holds the
// schedule switched off until somebody gives the question a default.
func TestAWXScheduleSurveyAnswersBecomeTheCopysDefaults(t *testing.T) {
	t.Parallel()
	const export = `{
	  "job_templates": [{"name": "Deploy", "playbook": "site.yml", "extra_vars": "channel: stable",
	    "survey_spec": {"spec": [
	      {"variable": "region", "question_name": "Region", "type": "text", "required": true},
	      {"variable": "release", "question_name": "Release", "type": "text", "default": "1.0"},
	      {"variable": "db_password", "question_name": "DB password", "type": "password"}
	    ]},
	    "related": {"schedules": [
	      {"name": "nightly", "rrule": "DTSTART:20260101T020000Z RRULE:FREQ=DAILY;INTERVAL=1",
	       "extra_data": {"region": "eu", "release": "1.2", "flag": "on"}},
	      {"name": "rotate", "rrule": "DTSTART:20260101T030000Z RRULE:FREQ=DAILY;INTERVAL=1",
	       "extra_data": {"region": "us", "db_password": "$encrypted$"}}
	    ]}}]}`
	plan, err := FromAWX([]byte(export), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	byName := map[string]*template.Template{}
	for _, tpl := range plan.Templates {
		byName[tpl.Name] = tpl
	}
	orig, nightly, rotate := byName["Deploy"], byName["Deploy (nightly)"], byName["Deploy (rotate)"]
	if orig == nil || nightly == nil || rotate == nil {
		t.Fatalf("templates = %v, want the original and a copy for each schedule", plan.Templates)
	}
	defaults := func(tpl *template.Template) map[string]any {
		out := map[string]any{}
		for _, f := range tpl.Survey {
			if f.Default != nil {
				out[f.Var] = f.Default
			}
		}
		return out
	}
	tests := []struct {
		// Template is the template read.
		Template *template.Template
		// WantDefaults are its questions' defaults.
		WantDefaults map[string]any
		// WantVars are its plain extra vars.
		WantVars map[string]any
	}{{ // Test 0: The template people launch by hand keeps its own survey.
		Template: orig, WantDefaults: map[string]any{"release": "1.0"},
		WantVars: map[string]any{"channel": "stable"},
	}, { // Test 1: The schedule's answers are the copy's defaults, its other variables plain vars.
		Template:     nightly,
		WantDefaults: map[string]any{"region": "eu", "release": "1.2"},
		WantVars:     map[string]any{"channel": "stable", "flag": "on"},
	}, { // Test 2: A secret answer is carried nowhere, neither as a default nor as a variable.
		Template: rotate, WantDefaults: map[string]any{"region": "us", "release": "1.0"},
		WantVars: map[string]any{"channel": "stable"},
	}}
	for testNum, test := range tests {
		if diff := cmp.Diff(test.WantDefaults, defaults(test.Template)); diff != "" {
			t.Errorf("test %d %s: survey defaults (-want +got):\n%s", testNum, test.Template.Name, diff)
		}
		if diff := cmp.Diff(test.WantVars, test.Template.ExtraVars, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("test %d %s: extra vars (-want +got):\n%s", testNum, test.Template.Name, diff)
		}
	}

	armed := map[string]bool{}
	for _, sc := range plan.Schedules {
		armed[sc.Name] = sc.Enabled
	}
	if !armed["nightly"] {
		t.Error("a schedule whose answers all came across arrived switched off")
	}
	if armed["rotate"] {
		t.Error("a schedule whose secret answer did not come across arrived armed")
	}
	w, ok := warningContaining(t, plan.Warnings, `schedule "rotate"`, "switched off", `"db_password"`,
		`"Deploy (rotate)"`)
	if !ok {
		t.Errorf("the held schedule was not reported with its question.\nwarnings: %v", plan.Warnings)
	}
	if strings.Contains(w, "$encrypted$") {
		t.Errorf("the report repeats the export's placeholder: %s", w)
	}

	// Fired with nobody there to answer, the copy of the armed schedule carries its answers, and the
	// held one has nothing for the secret question, which is why it waits switched off.
	vars, _, err := nightly.UnattendedVars()
	if err != nil {
		t.Errorf("UnattendedVars(nightly) error = %v, want its answers to fill every question", err)
	}
	if vars["region"] != "eu" {
		t.Errorf("the nightly copy fires with %v, want the schedule's answers", vars)
	}
	if _, sealed, err := rotate.UnattendedVars(); err != nil || len(sealed) != 0 {
		t.Errorf("UnattendedVars(rotate) = sealed %v, error %v, want no sealed answer to fire with "+
			"until the question is given a default", sealed, err)
	}
}

// TestAWXScheduleHeldHereIsNotCalledSwitchedOffAtTheSource pins the report for a schedule the import
// holds back. It is switched off because part of what it overrides did not come across, and the
// report has to say that. It used to say the schedule was switched off at the source too, which
// sent an operator back to AWX to look for a setting that was never there.
func TestAWXScheduleHeldHereIsNotCalledSwitchedOffAtTheSource(t *testing.T) {
	t.Parallel()
	const export = `{"job_templates": [{"name": "Deploy", "playbook": "site.yml",
	  "survey_spec": {"spec": [{"variable": "token", "type": "password"}]},
	  "related": {"schedules": [
	    {"name": "rotate", "rrule": "DTSTART:20260101T030000Z RRULE:FREQ=DAILY;INTERVAL=1",
	     "extra_data": {"token": "$encrypted$"}},
	    {"name": "elsewhere", "rrule": "DTSTART:20260101T040000Z RRULE:FREQ=DAILY;INTERVAL=1",
	     "inventory": "missing"}
	  ]}}]}`
	plan, err := FromAWX([]byte(export), importNow)
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	for _, name := range []string{"rotate", "elsewhere"} {
		if w, ok := warningContaining(t, plan.Warnings, `schedule "`+name+`"`, "at the source"); ok {
			t.Errorf("schedule %q held by the import is reported as switched off at the source: %s",
				name, w)
		}
		if _, ok := warningContaining(t, plan.Warnings, `schedule "`+name+`"`, "arrives switched off"); !ok {
			t.Errorf("schedule %q held by the import was not reported.\nwarnings: %v", name,
				plan.Warnings)
		}
	}
}

// TestAScheduleNobodyCanAnswerIsNamedInTheReport pins the note for every importer's schedules. A
// schedule fires with each survey question's default and stops on a required question with none, so
// a Rundeck job whose required option has no default, scheduled as it was in Rundeck, stops on
// every fire here. The report says so before the first one, and files it as something to review,
// since the schedule itself came across.
func TestAScheduleNobodyCanAnswerIsNamedInTheReport(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Options is the job's options block.
		Options string
		// WantNote is whether the report must name the schedule.
		WantNote bool
	}{{ // Test 0: A required option with no default.
		Options: "    - name: release\n      required: true\n", WantNote: true,
	}, { // Test 1: A required option with a default, which a fire takes.
		Options: "    - name: release\n      required: true\n      value: '1.0'\n",
	}, { // Test 2: An optional option with no default, which a fire leaves out.
		Options: "    - name: release\n",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			plan := rundeckPlan(t, "prod", "- name: Nightly\n  options:\n"+test.Options+
				"  schedule:\n    crontab: '0 0 2 * * ? *'\n  sequence:\n    commands:\n"+
				"      - exec: nightly.sh\n")
			w, ok := warningContaining(t, plan.Warnings, `schedule "Nightly"`, `"Nightly"`, "release",
				"no usable default")
			if ok != test.WantNote {
				t.Fatalf("noted = %v, want %v.\nwarnings: %v", ok, test.WantNote, plan.Warnings)
			}
			if !ok {
				return
			}
			report := plan.Report()
			if slices.Contains(report.LeftOut, w) || !slices.Contains(report.NeedsReview, w) {
				t.Errorf("the note is filed as left out %v, want it listed for review", report.LeftOut)
			}
		})
	}
}
