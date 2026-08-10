package importer

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// testNow is the fixed time imported objects are stamped with.
var testNow = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

// TestFromRundeckJob covers the shape of an imported job: name qualification, the step sequence
// rendered as one script, options mapped to a survey, dispatch thread count as forks, and the
// timeout converted to seconds.
func TestFromRundeckJob(t *testing.T) {
	t.Parallel()
	export := `
- name: Deploy web
  group: prod/deploys
  project: infra
  description: Ship the web tier
  timeout: '1h30m'
  nodefilters:
    filter: 'tags: web'
    dispatch:
      threadcount: 12
  options:
    - name: environment
      description: Where to ship
      value: staging
      values: [staging, prod]
      enforced: true
      required: true
    - name: notes
  sequence:
    keepgoing: false
    commands:
      - description: Pull the build
        exec: /usr/bin/fetch-build
      - script: |
          #!/bin/bash
          echo deploying
`
	plan, err := FromRundeck("prod-hosts")([]byte(export), testNow)
	if err != nil {
		t.Fatalf("FromRundeck() error = %v", err)
	}
	if len(plan.Templates) != 1 {
		t.Fatalf("templates = %d, want 1 (warnings %v)", len(plan.Templates), plan.Warnings)
	}
	tmpl := plan.Templates[0]
	if tmpl.Name != "prod/deploys/Deploy web" {
		t.Errorf("name = %q, want the group-qualified name", tmpl.Name)
	}
	if tmpl.Tool != "bash" {
		t.Errorf("tool = %q, want bash", tmpl.Tool)
	}
	if tmpl.Inventory != "prod-hosts" {
		t.Errorf("inventory = %q, want the one named on the command line", tmpl.Inventory)
	}
	if tmpl.Forks != 12 {
		t.Errorf("forks = %d, want the dispatch thread count 12", tmpl.Forks)
	}
	if tmpl.Timeout != 5400 {
		t.Errorf("timeout = %d, want 5400 seconds for 1h30m", tmpl.Timeout)
	}
	// The sequence stops at the first failure in Rundeck, so the script must too.
	if !hasShellLine(tmpl.Command, "set -e") {
		t.Errorf("command does not set -e for a sequence that stops on failure:\n%s", tmpl.Command)
	}
	for _, want := range []string{"/usr/bin/fetch-build", "echo deploying", "# Pull the build"} {
		if !strings.Contains(tmpl.Command, want) {
			t.Errorf("command is missing %q:\n%s", want, tmpl.Command)
		}
	}
	if len(tmpl.Survey) != 2 {
		t.Fatalf("survey fields = %d, want 2", len(tmpl.Survey))
	}
	env := tmpl.Survey[0]
	if env.Type != "choice" {
		t.Errorf("enforced option type = %q, want choice", env.Type)
	}
	if !env.Required || env.Default != "staging" || env.Help != "Where to ship" {
		t.Errorf("option mapped as %+v, want required with a staging default and its description", env)
	}
}

// TestFromRundeckSecureOptionRefused proves a secure option is never imported as a survey field.
//
// Rundeck keeps a secure option's value obscured. A survey field here is plain text whose answer is
// stored on the run and injected as an extra var, so importing one would quietly turn a password
// prompt into a stored plaintext value. The importer must drop it and say so.
func TestFromRundeckSecureOptionRefused(t *testing.T) {
	t.Parallel()
	export := `
- name: Rotate
  options:
    - name: db_password
      secure: true
      required: true
    - name: region
  sequence:
    commands:
      - exec: rotate.sh
`
	plan, err := FromRundeck("hosts")([]byte(export), testNow)
	if err != nil {
		t.Fatalf("FromRundeck() error = %v", err)
	}
	if len(plan.Templates) != 1 {
		t.Fatalf("templates = %d, want 1", len(plan.Templates))
	}
	for _, f := range plan.Templates[0].Survey {
		if f.Var == "db_password" {
			t.Fatal("a secure option was imported as a survey field, which stores the secret in plain text")
		}
	}
	if len(plan.Templates[0].Survey) != 1 {
		t.Errorf("survey fields = %d, want only the non-secure one", len(plan.Templates[0].Survey))
	}
	var told bool
	for _, w := range plan.Warnings {
		if strings.Contains(w, "db_password") && strings.Contains(w, "secure") {
			told = true
		}
	}
	if !told {
		t.Errorf("dropping the secure option was not reported; warnings = %v", plan.Warnings)
	}
}

// TestRundeckQuartzWeekday proves Quartz weekday numbering is renumbered for standard cron.
//
// Quartz counts Sunday as one through Saturday as seven; cron counts Sunday as zero through Saturday
// as six. Copying the field across without renumbering shifts every weekly job by one day, which is
// the kind of migration bug nobody notices until a Monday deploy has been running on Sundays.
func TestRundeckQuartzWeekday(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Crontab string
		Want    string
		WantOK  bool
	}{ // Test 0: Quartz Monday, 2, becomes cron Monday, 1.
		{"0 30 2 ? * 2 *", "30 2 * * 1", true},
		// Test 1: A weekday range shifts at both ends, so 2-6 weekdays becomes 1-5.
		{"0 0 9 ? * 2-6", "0 9 * * 1-5", true},
		// Test 2: A list shifts every member.
		{"0 0 9 ? * 1,7", "0 9 * * 0,6", true},
		// Test 3: Day names are the same in both and pass through untouched.
		{"0 0 9 ? * WED", "0 9 * * WED", true},
		// Test 4: A day-of-month schedule leaves the weekday as the wildcard Quartz spells '?'.
		{"0 0 3 15 * ?", "0 3 15 * *", true},
		// Test 5: A five field expression is already standard cron.
		{"0 3 * * 1", "0 3 * * 1", true},
		// Test 6: The nth-weekday form has no cron equivalent and must be refused, not guessed.
		{"0 0 9 ? * 6#3", "", false},
		// Test 7: 'L' for the last weekday has no cron equivalent either.
		{"0 0 9 ? * 6L", "", false},
		// Test 8: A weekday outside the Quartz range is refused rather than shifted to nonsense.
		{"0 0 9 ? * 9", "", false},
		// Test 9: A field count that is neither cron nor Quartz is refused.
		{"0 0 9 ?", "", false},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Crontab), func(t *testing.T) {
			t.Parallel()
			plan := &Plan{}
			got, ok := plan.quartzToCron(test.Crontab, "job")
			if ok != test.WantOK {
				t.Fatalf("quartzToCron(%q) ok = %v, want %v (warnings %v)",
					test.Crontab, ok, test.WantOK, plan.Warnings)
			}
			if ok && got != test.Want {
				t.Errorf("quartzToCron(%q) = %q, want %q", test.Crontab, got, test.Want)
			}
			if !ok && len(plan.Warnings) == 0 {
				t.Error("a refused schedule reported no warning, so the operator is not told")
			}
		})
	}
}

// TestFromRundeckSchedule covers both schedule forms reaching the plan as a working schedule whose
// first fire time is stamped, and a disabled one being left out.
func TestFromRundeckSchedule(t *testing.T) {
	t.Parallel()
	export := `
- name: Nightly
  schedule:
    crontab: '0 0 2 * * ? *'
  sequence:
    commands:
      - exec: nightly.sh
- name: Structured
  schedule:
    time:
      hour: '4'
      minute: '15'
      seconds: '0'
    month: '*'
    dayofmonth:
      day: '*'
    weekday:
      day: '?'
  sequence:
    commands:
      - exec: structured.sh
- name: Paused
  scheduleEnabled: false
  schedule:
    crontab: '0 0 5 * * ? *'
  sequence:
    commands:
      - exec: paused.sh
`
	plan, err := FromRundeck("hosts")([]byte(export), testNow)
	if err != nil {
		t.Fatalf("FromRundeck() error = %v", err)
	}
	if len(plan.Templates) != 3 {
		t.Fatalf("templates = %d, want 3", len(plan.Templates))
	}
	if len(plan.Schedules) != 2 {
		t.Fatalf("schedules = %d, want 2 (the paused one is left out); warnings %v",
			len(plan.Schedules), plan.Warnings)
	}
	byName := map[string]string{}
	for _, sc := range plan.Schedules {
		byName[sc.Name] = sc.Cron
		// A schedule with no next fire time is skipped by the scheduler forever, so an imported one
		// that never runs would look successful and do nothing.
		if sc.NextRunAt == nil {
			t.Errorf("schedule %q has no next fire time, so it would never run", sc.Name)
		}
		if sc.TemplateID == "" {
			t.Errorf("schedule %q is not wired to a template", sc.Name)
		}
	}
	if byName["Nightly"] != "0 2 * * *" {
		t.Errorf("Nightly cron = %q, want 0 2 * * *", byName["Nightly"])
	}
	if byName["Structured"] != "15 4 * * *" {
		t.Errorf("Structured cron = %q, want 15 4 * * *", byName["Structured"])
	}
}

// TestFromRundeckUnmappableSteps proves a step with no equivalent is reported rather than silently
// dropped, and that a job made only of such steps is skipped instead of importing as a template that
// reports success without running anything.
func TestFromRundeckUnmappableSteps(t *testing.T) {
	t.Parallel()
	export := `
- name: Chained
  sequence:
    commands:
      - jobref:
          name: Other job
          group: g
      - exec: real.sh
- name: Only references
  sequence:
    commands:
      - jobref:
          name: Another
`
	plan, err := FromRundeck("hosts")([]byte(export), testNow)
	if err != nil {
		t.Fatalf("FromRundeck() error = %v", err)
	}
	if len(plan.Templates) != 1 {
		t.Fatalf("templates = %d, want only the job with a runnable step", len(plan.Templates))
	}
	if plan.Templates[0].Name != "Chained" {
		t.Errorf("imported %q, want Chained", plan.Templates[0].Name)
	}
	var reportedRef, reportedSkip bool
	for _, w := range plan.Warnings {
		if strings.Contains(w, "Other job") {
			reportedRef = true
		}
		if strings.Contains(w, "Only references") && strings.Contains(w, "skipped") {
			reportedSkip = true
		}
	}
	if !reportedRef {
		t.Errorf("the dropped job reference was not reported; warnings = %v", plan.Warnings)
	}
	if !reportedSkip {
		t.Errorf("the skipped job was not reported; warnings = %v", plan.Warnings)
	}
}

// TestFromRundeckKeepGoing proves a sequence that continued past failures does not get set -e, since
// that would change what the migrated job does.
func TestFromRundeckKeepGoing(t *testing.T) {
	t.Parallel()
	export := `
- name: Best effort
  sequence:
    keepgoing: true
    commands:
      - exec: one.sh
      - exec: two.sh
`
	plan, err := FromRundeck("hosts")([]byte(export), testNow)
	if err != nil {
		t.Fatalf("FromRundeck() error = %v", err)
	}
	if len(plan.Templates) != 1 {
		t.Fatalf("templates = %d, want 1", len(plan.Templates))
	}
	if hasShellLine(plan.Templates[0].Command, "set -e") {
		t.Errorf("a keepgoing sequence was given set -e, which stops it early:\n%s",
			plan.Templates[0].Command)
	}
}

// hasShellLine reports whether the script carries want as a line of its own, ignoring comments. A
// plain substring search would match the explanatory comment the importer writes about set -e.
func hasShellLine(script, want string) bool {
	for line := range strings.SplitSeq(script, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// TestFromRundeckMalformed covers inputs that must not panic or import garbage.
func TestFromRundeckMalformed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Body    string
		WantErr bool
	}{ // Test 0: A document that is not a job list is an error, not a silent empty plan.
		{"not a list", "just a string", true},
		// Test 1: An empty list imports nothing and says so.
		{"empty list", "[]", false},
		// Test 2: A wrapped list is accepted, since some exports nest under a jobs key.
		{"wrapped", "jobs:\n  - name: X\n    sequence:\n      commands:\n        - exec: x.sh\n", false},
		// Test 3: A job with no name is reported rather than imported unnamed.
		{"no name", "- sequence:\n    commands:\n      - exec: x.sh\n", false},
		// Test 4: JSON is valid YAML, so a JSON export reads too.
		{"json", `[{"name":"J","sequence":{"commands":[{"exec":"j.sh"}]}}]`, false},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			plan, err := FromRundeck("hosts")([]byte(test.Body), testNow)
			if (err != nil) != test.WantErr {
				t.Fatalf("FromRundeck() error = %v, wantErr %v", err, test.WantErr)
			}
			if err != nil {
				return
			}
			if plan == nil {
				t.Fatal("plan is nil with no error")
			}
			for _, tmpl := range plan.Templates {
				if tmpl.Name == "" {
					t.Error("a template was imported with no name")
				}
			}
		})
	}
}
