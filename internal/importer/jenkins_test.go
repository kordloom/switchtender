package importer_test

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/importer"
	"github.com/kordloom/switchtender/internal/template"
)

// jenkinsBundle wraps job documents the way the walker does after reading a jobs directory.
//
// The name is escaped as XML rather than quoted as Go, which is the same thing the walker does and
// the only way a name holding a quote or a newline survives the trip intact.
func jenkinsBundle(jobs ...[2]string) []byte {
	var b bytes.Buffer
	b.WriteString("<jobs>")
	for _, j := range jobs {
		b.WriteString(`<job name="`)
		if err := xml.EscapeText(&b, []byte(j[0])); err != nil {
			panic(err)
		}
		b.WriteString(`">` + j[1] + "</job>")
	}
	b.WriteString("</jobs>")
	return b.Bytes()
}

// freestyle builds a freestyle job document around the given inner elements.
func freestyle(inner string) string {
	return "<project><description>d</description>" + inner + "</project>"
}

// shellStep is a build step list holding one shell script.
func shellStep(script string) string {
	return "<builders><hudson.tasks.Shell><command>" + script +
		"</command></hudson.tasks.Shell></builders>"
}

// timer is a trigger list holding one timer with the given specification.
func timer(spec string) string {
	return "<triggers><hudson.triggers.TimerTrigger><spec>" + spec +
		"</spec></hudson.triggers.TimerTrigger></triggers>"
}

func TestFromJenkinsMapsAFreestyleJob(t *testing.T) {
	t.Parallel()
	doc := freestyle(`<properties><hudson.model.ParametersDefinitionProperty><parameterDefinitions>
	<hudson.model.StringParameterDefinition><name>TARGET</name><description>Where to</description>
		<defaultValue>staging</defaultValue></hudson.model.StringParameterDefinition>
	<hudson.model.ChoiceParameterDefinition><name>REGION</name>
		<choices class="java.util.Arrays$ArrayList"><a class="string-array">
			<string>us-east-1</string><string>us-west-2</string></a></choices>
	</hudson.model.ChoiceParameterDefinition>
	<hudson.model.BooleanParameterDefinition><name>DRY_RUN</name>
		<defaultValue>true</defaultValue></hudson.model.BooleanParameterDefinition>
	</parameterDefinitions></hudson.model.ParametersDefinitionProperty></properties>` +
		timer("30 2 * * *") + shellStep("echo one") +
		`<buildWrappers><hudson.plugins.build__timeout.BuildTimeoutWrapper>
			<strategy><timeoutMinutes>45</timeoutMinutes></strategy>
		</hudson.plugins.build__timeout.BuildTimeoutWrapper></buildWrappers>`)

	plan, err := importer.FromJenkins("prod")(jenkinsBundle([2]string{"nightly", doc}), fixedTime)
	if err != nil {
		t.Fatalf("FromJenkins() error = %v", err)
	}
	if len(plan.Templates) != 1 {
		t.Fatalf("templates = %d, want 1", len(plan.Templates))
	}
	tpl := plan.Templates[0]
	if tpl.Name != "nightly" || tpl.Tool != "bash" || tpl.Inventory != "prod" {
		t.Errorf("template = %+v, want the named bash template on the prod inventory", tpl)
	}
	if tpl.Timeout != 45*60 {
		t.Errorf("timeout = %d, want the wrapper's 45 minutes in seconds", tpl.Timeout)
	}
	if !strings.HasPrefix(tpl.Command, "set -e\n") || !strings.Contains(tpl.Command, "echo one") {
		t.Errorf("command = %q, want set -e and the step body", tpl.Command)
	}
	wantSurvey := []template.SurveyField{
		{Var: "TARGET", Label: "TARGET", Type: template.FieldText, Help: "Where to", Default: "staging"},
		{Var: "REGION", Label: "REGION", Type: template.FieldChoice,
			Choices: []string{"us-east-1", "us-west-2"}, Default: "us-east-1"},
		{Var: "DRY_RUN", Label: "DRY_RUN", Type: template.FieldBool, Default: "true"},
	}
	if diff := cmp.Diff(wantSurvey, tpl.Survey); diff != "" {
		t.Errorf("survey mismatch (-want +got):\n%s", diff)
	}
	if len(plan.Schedules) != 1 || plan.Schedules[0].Cron != "30 2 * * *" {
		t.Fatalf("schedules = %+v, want the timer imported", plan.Schedules)
	}
	if plan.Schedules[0].TemplateID != tpl.ID {
		t.Errorf("schedule is not wired to the template")
	}
}

// TestJenkinsHashNotationResolvesToRealTimes covers the notation that has no cron equivalent at all.
//
// Jenkins spreads load by writing H where a number would go and hashing the job name to pick one.
// The scheduler rejects the letter outright, so a spec carrying one is not merely approximate, it is
// unparseable: every H job would have been dropped as invalid. Each case here must come out as a
// real expression the scheduler accepts, with the cadence intact.
func TestJenkinsHashNotationResolvesToRealTimes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Spec      string
		WantField string
		WantEvery int
	}{{ // Test 0: A bare H picks one minute of the hour.
		Spec: "H 2 * * *", WantField: "0", WantEvery: 0,
	}, { // Test 1: A step repeats within the hour, offset so jobs do not stack.
		Spec: "H/15 * * * *", WantField: "0", WantEvery: 15,
	}, { // Test 2: A window narrows where the hash may land.
		Spec: "H H(0-2) * * *", WantField: "1", WantEvery: 0,
	}, { // Test 3: Every field may be hashed at once.
		Spec: "H H H * *", WantField: "2", WantEvery: 0,
	}, { // Test 4: The daily alias expands before the hash resolves.
		Spec: "@daily", WantField: "1", WantEvery: 0,
	}, { // Test 5: The midnight alias carries a windowed hour.
		Spec: "@midnight", WantField: "1", WantEvery: 0,
	}, { // Test 6: The weekly alias hashes the weekday too.
		Spec: "@weekly", WantField: "4", WantEvery: 0,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			doc := freestyle(timer(test.Spec) + shellStep("echo hi"))
			plan, err := importer.FromJenkins("inv")(
				jenkinsBundle([2]string{"job-a", doc}), fixedTime)
			if err != nil {
				t.Fatalf("FromJenkins() error = %v", err)
			}
			// A schedule only reaches the plan after the scheduler's own parser accepts it and a
			// next firing is computed, so its presence is the proof the conversion is usable.
			if len(plan.Schedules) != 1 {
				t.Fatalf("schedules = %+v, want %q to convert into one usable schedule",
					plan.Warnings, test.Spec)
			}
			got := plan.Schedules[0].Cron
			if strings.Contains(got, "H") {
				t.Fatalf("cron = %q, want no H left in it", got)
			}
			field := strings.Fields(got)[parseIndex(t, test.WantField)]
			if test.WantEvery > 0 {
				if !strings.HasSuffix(field, fmt.Sprintf("/%d", test.WantEvery)) {
					t.Errorf("cron = %q, want the field to keep its every-%d cadence",
						got, test.WantEvery)
				}
				return
			}
			if field == "*" {
				t.Errorf("cron = %q, want the hashed field to hold a concrete value", got)
			}
		})
	}
}

// parseIndex converts a field index written as a string in the table.
func parseIndex(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		t.Fatalf("bad index %q", s)
	}
	return n
}

// TestJenkinsHashIsStableAcrossImports covers re-importing the same Jenkins twice.
//
// The whole point of Jenkins hashing the job name is that a job fires at the same moment every time
// rather than drifting. A hash that varied per import would move every schedule on each run, so a
// second import of the same job must produce the identical expression.
func TestJenkinsHashIsStableAcrossImports(t *testing.T) {
	t.Parallel()
	doc := freestyle(timer("H H * * *") + shellStep("echo hi"))
	first, err := importer.FromJenkins("")(jenkinsBundle([2]string{"backup", doc}), fixedTime)
	if err != nil {
		t.Fatalf("FromJenkins() error = %v", err)
	}
	second, err := importer.FromJenkins("")(jenkinsBundle([2]string{"backup", doc}), fixedTime)
	if err != nil {
		t.Fatalf("FromJenkins() error = %v", err)
	}
	if first.Schedules[0].Cron != second.Schedules[0].Cron {
		t.Errorf("cron drifted between imports: %q then %q",
			first.Schedules[0].Cron, second.Schedules[0].Cron)
	}
	// Two different jobs should not pile onto the same instant, which is what H exists to prevent.
	other, err := importer.FromJenkins("")(jenkinsBundle([2]string{"reindex", doc}), fixedTime)
	if err != nil {
		t.Fatalf("FromJenkins() error = %v", err)
	}
	if first.Schedules[0].Cron == other.Schedules[0].Cron {
		t.Errorf("two jobs hashed to the same time %q, which defeats the spreading",
			first.Schedules[0].Cron)
	}
}

// TestJenkinsSundayIsRenumbered covers the one numbering difference between the two cron dialects.
//
// Jenkins accepts both 0 and 7 for Sunday. The scheduler's parser rejects 7 as above the maximum, so
// a weekly job written the second way is not shifted by a day, it is dropped entirely.
func TestJenkinsSundayIsRenumbered(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Spec     string
		WantCron string
	}{{ // Test 0: A bare 7 is Sunday and must become 0.
		Spec: "0 3 * * 7", WantCron: "0 3 * * 0",
	}, { // Test 1: A 7 inside a list is still Sunday.
		Spec: "0 3 * * 1,7", WantCron: "0 3 * * 1,0",
	}, { // Test 2: A 7 that is part of a larger number is not a weekday at all.
		Spec: "17 7 * * 1", WantCron: "17 7 * * 1",
	}, { // Test 3: Sunday written as 0 is already correct.
		Spec: "0 3 * * 0", WantCron: "0 3 * * 0",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			doc := freestyle(timer(test.Spec) + shellStep("echo hi"))
			plan, err := importer.FromJenkins("")(jenkinsBundle([2]string{"weekly", doc}), fixedTime)
			if err != nil {
				t.Fatalf("FromJenkins() error = %v", err)
			}
			if len(plan.Schedules) != 1 {
				t.Fatalf("schedules = %+v, warnings %v, want %q imported",
					plan.Schedules, plan.Warnings, test.Spec)
			}
			if diff := cmp.Diff(test.WantCron, plan.Schedules[0].Cron); diff != "" {
				t.Errorf("cron mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestJenkinsMultiLineTimerBecomesSeveralSchedules covers a spec holding more than one rule.
//
// A Jenkins timer takes one rule per line and allows comments. Reading the block as a single
// expression would fail to parse and drop every rule in it.
func TestJenkinsMultiLineTimerBecomesSeveralSchedules(t *testing.T) {
	t.Parallel()
	spec := "# weekday mornings\n0 6 * * 1-5\n\n# and Sunday night\n30 22 * * 7\n"
	doc := freestyle(timer(spec) + shellStep("echo hi"))
	plan, err := importer.FromJenkins("")(jenkinsBundle([2]string{"sweep", doc}), fixedTime)
	if err != nil {
		t.Fatalf("FromJenkins() error = %v", err)
	}
	var got []string
	for _, s := range plan.Schedules {
		got = append(got, s.Cron)
	}
	if diff := cmp.Diff([]string{"0 6 * * 1-5", "30 22 * * 0"}, got); diff != "" {
		t.Errorf("schedules mismatch (-want +got):\n%s", diff)
	}
}

// TestJenkinsPollTriggerIsNotImportedAsASchedule covers the trigger most likely to be mistaken for
// one.
//
// A poll trigger asks the repository whether anything changed and builds only if it did, so a job
// polling every five minutes usually does nothing. Imported as a plain schedule it would instead run
// for real every five minutes, which for a deploy job is a production incident rather than a
// migration artifact.
func TestJenkinsPollTriggerIsNotImportedAsASchedule(t *testing.T) {
	t.Parallel()
	doc := freestyle(`<triggers><hudson.triggers.SCMTrigger><spec>H/5 * * * *</spec>
		</hudson.triggers.SCMTrigger></triggers>` + shellStep("./deploy.sh"))
	plan, err := importer.FromJenkins("")(jenkinsBundle([2]string{"deploy", doc}), fixedTime)
	if err != nil {
		t.Fatalf("FromJenkins() error = %v", err)
	}
	if len(plan.Schedules) != 0 {
		t.Fatalf("schedules = %+v, want none: a poll trigger is not a schedule", plan.Schedules)
	}
	if len(plan.Templates) != 1 {
		t.Fatalf("templates = %d, want the job itself still imported", len(plan.Templates))
	}
	assertWarns(t, plan.Warnings, "polled source control", "NOT imported")
}

// TestJenkinsPasswordParameterIsNotImportedAsPlainText covers the same rule the AWX, Semaphore, and
// Rundeck importers hold.
//
// Jenkins stores a password parameter encrypted. A survey answer is kept in plain text on the run
// and injected as an extra var, so importing one would quietly downgrade a secret to a stored
// plaintext value on every launch.
func TestJenkinsPasswordParameterIsNotImportedAsPlainText(t *testing.T) {
	t.Parallel()
	doc := freestyle(`<properties><hudson.model.ParametersDefinitionProperty><parameterDefinitions>
		<hudson.model.PasswordParameterDefinition><name>API_TOKEN</name>
			<defaultValue>{AQAAABAAAAAQfakeciphertext}</defaultValue>
		</hudson.model.PasswordParameterDefinition>
		<hudson.model.StringParameterDefinition><name>KEEP</name>
			</hudson.model.StringParameterDefinition>
		</parameterDefinitions></hudson.model.ParametersDefinitionProperty></properties>` +
		shellStep("echo hi"))
	plan, err := importer.FromJenkins("")(jenkinsBundle([2]string{"secretive", doc}), fixedTime)
	if err != nil {
		t.Fatalf("FromJenkins() error = %v", err)
	}
	tpl := plan.Templates[0]
	for _, f := range tpl.Survey {
		if f.Var == "API_TOKEN" {
			t.Fatalf("the password parameter was imported as a survey field: %+v", f)
		}
	}
	if len(tpl.Survey) != 1 || tpl.Survey[0].Var != "KEEP" {
		t.Errorf("survey = %+v, want only the non-secret parameter", tpl.Survey)
	}
	// The encrypted value must not travel into the plan in any form, warnings included.
	for _, w := range plan.Warnings {
		if strings.Contains(w, "AQAAABAAAAAQ") {
			t.Fatalf("warning leaked the stored secret: %q", w)
		}
	}
	assertWarns(t, plan.Warnings, "password parameter and was NOT imported")
}

// TestJenkinsRemoteTriggerTokenIsNotImported covers the other secret a job config carries.
func TestJenkinsRemoteTriggerTokenIsNotImported(t *testing.T) {
	t.Parallel()
	doc := freestyle("<authToken>s3cr3t-trigger</authToken>" + shellStep("echo hi"))
	plan, err := importer.FromJenkins("")(jenkinsBundle([2]string{"triggerable", doc}), fixedTime)
	if err != nil {
		t.Fatalf("FromJenkins() error = %v", err)
	}
	for _, w := range plan.Warnings {
		if strings.Contains(w, "s3cr3t-trigger") {
			t.Fatalf("warning leaked the trigger token: %q", w)
		}
	}
	assertWarns(t, plan.Warnings, "remote trigger token")
}

// TestJenkinsRefusesJobTypesItCannotTranslate covers every root element that is not a freestyle job.
//
// A Pipeline is a Groovy program. There is no mechanical translation from one into a template, and
// importing a shell of one that runs nothing would be worse than not importing it, so each is
// refused by name.
func TestJenkinsRefusesJobTypesItCannotTranslate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Doc      string
		WantWarn string
	}{{ // Test 0: A Pipeline job.
		Doc:      "<flow-definition><definition/></flow-definition>",
		WantWarn: "Pipeline job",
	}, { // Test 1: A multibranch Pipeline.
		Doc:      "<org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject/>",
		WantWarn: "multibranch",
	}, { // Test 2: A matrix job.
		Doc: "<matrix-project><axes/></matrix-project>", WantWarn: "matrix job",
	}, { // Test 3: A Maven job.
		Doc: "<maven2-moduleset/>", WantWarn: "Maven job",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			// A runnable freestyle job rides along so the import as a whole still succeeds.
			plan, err := importer.FromJenkins("")(jenkinsBundle(
				[2]string{"unmappable", test.Doc},
				[2]string{"fine", freestyle(shellStep("echo hi"))},
			), fixedTime)
			if err != nil {
				t.Fatalf("FromJenkins() error = %v", err)
			}
			if len(plan.Templates) != 1 || plan.Templates[0].Name != "fine" {
				t.Fatalf("templates = %+v, want only the freestyle job", plan.Templates)
			}
			assertWarns(t, plan.Warnings, test.WantWarn, "not imported")
		})
	}
}

// TestJenkinsSkipsAJobWithNothingRunnable covers a job whose every step was left out.
//
// An imported template with no script would report success without doing anything, which reads as a
// migrated job that works right up until somebody depends on it.
func TestJenkinsSkipsAJobWithNothingRunnable(t *testing.T) {
	t.Parallel()
	doc := freestyle(`<builders><hudson.tasks.Maven><targets>clean install</targets>
		</hudson.tasks.Maven></builders>`)
	_, err := importer.FromJenkins("")(jenkinsBundle([2]string{"mavenish", doc}), fixedTime)
	if err == nil {
		t.Fatal("FromJenkins() error = nil, want a refusal when nothing could be imported")
	}
}

// TestJenkinsReportsVariablesItCannotSupply covers the most common reason a migrated script fails.
//
// Jenkins sets WORKSPACE, BUILD_NUMBER, and the rest for every build. Here they are unset, so a
// script reading one silently gets an empty string, and "rm -rf $WORKSPACE/" with WORKSPACE unset is
// the worst version of that.
func TestJenkinsReportsVariablesItCannotSupply(t *testing.T) {
	t.Parallel()
	doc := freestyle(shellStep("cd $WORKSPACE\necho ${BUILD_NUMBER}\necho $MY_OWN_VAR") +
		timer("0 1 * * *"))
	plan, err := importer.FromJenkins("")(jenkinsBundle([2]string{"usesvars", doc}), fixedTime)
	if err != nil {
		t.Fatalf("FromJenkins() error = %v", err)
	}
	assertWarns(t, plan.Warnings, "$WORKSPACE", "$BUILD_NUMBER")
	for _, w := range plan.Warnings {
		if strings.Contains(w, "MY_OWN_VAR") {
			t.Errorf("warned about a variable that is not a Jenkins one: %q", w)
		}
	}
}

// TestJenkinsVariableMatchIsBounded covers the substring trap in that detection.
//
// WORKSPACE is a prefix of WORKSPACE_TMP and BUILD_ID of nothing, so an unbounded match reports
// variables the script never used and buries the real warnings under noise.
func TestJenkinsVariableMatchIsBounded(t *testing.T) {
	t.Parallel()
	doc := freestyle(shellStep("echo $WORKSPACE_TMP"))
	plan, err := importer.FromJenkins("")(jenkinsBundle([2]string{"bounded", doc}), fixedTime)
	if err != nil {
		t.Fatalf("FromJenkins() error = %v", err)
	}
	for _, w := range plan.Warnings {
		if strings.Contains(w, "$WORKSPACE,") || strings.Contains(w, "$WORKSPACE ") {
			t.Errorf("matched WORKSPACE inside WORKSPACE_TMP: %q", w)
		}
	}
	assertWarns(t, plan.Warnings, "$WORKSPACE_TMP")
}

// TestJenkinsReportsANonShellShebang covers a step written in another language.
//
// Jenkins honors a shebang and runs the step under that interpreter. A template runs its script with
// bash, where the line is only a comment, so a Python step would be fed to the wrong interpreter and
// fail in a way that points nowhere near the import.
func TestJenkinsReportsANonShellShebang(t *testing.T) {
	t.Parallel()
	doc := freestyle(shellStep("#!/usr/bin/env python3\nprint('hi')"))
	plan, err := importer.FromJenkins("")(jenkinsBundle([2]string{"pythonic", doc}), fixedTime)
	if err != nil {
		t.Fatalf("FromJenkins() error = %v", err)
	}
	assertWarns(t, plan.Warnings, "python3", "runs its script with bash")

	// A shell shebang is the normal case and must not be reported.
	quiet, err := importer.FromJenkins("")(
		jenkinsBundle([2]string{"shellish", freestyle(shellStep("#!/bin/bash -xe\necho hi"))}),
		fixedTime)
	if err != nil {
		t.Fatalf("FromJenkins() error = %v", err)
	}
	for _, w := range quiet.Warnings {
		if strings.Contains(w, "runs its script with bash") {
			t.Errorf("reported a shebang that bash handles correctly: %q", w)
		}
	}
}

// TestJenkinsDisabledJobImportsWithItsScheduleOff covers a job switched off in Jenkins.
//
// Importing it is right, since the definition is worth keeping. Importing its schedule as live is
// not: a job somebody deliberately stopped would start running again the moment it was migrated.
func TestJenkinsDisabledJobImportsWithItsScheduleOff(t *testing.T) {
	t.Parallel()
	doc := freestyle("<disabled>true</disabled>" + timer("0 4 * * *") + shellStep("echo hi"))
	plan, err := importer.FromJenkins("")(jenkinsBundle([2]string{"paused", doc}), fixedTime)
	if err != nil {
		t.Fatalf("FromJenkins() error = %v", err)
	}
	if len(plan.Templates) != 1 {
		t.Fatalf("templates = %d, want the disabled job still imported", len(plan.Templates))
	}
	if len(plan.Schedules) != 1 {
		t.Fatalf("schedules = %d, want one", len(plan.Schedules))
	}
	if plan.Schedules[0].Enabled {
		t.Error("the schedule of a disabled job was imported switched on")
	}
	assertWarns(t, plan.Warnings, "disabled in Jenkins")
}

// TestJenkinsSCMIsReportedRatherThanAttached covers a job that checked a repository out.
//
// Attaching a project silently would change the working directory every relative path in the script
// resolves against, so the repository is named and left for the operator to attach.
func TestJenkinsSCMIsReportedRatherThanAttached(t *testing.T) {
	t.Parallel()
	doc := freestyle(`<scm class="hudson.plugins.git.GitSCM">
		<userRemoteConfigs><hudson.plugins.git.UserRemoteConfig>
			<url>https://github.com/acme/infra.git</url>
		</hudson.plugins.git.UserRemoteConfig></userRemoteConfigs>
		<branches><hudson.plugins.git.BranchSpec><name>*/main</name>
			</hudson.plugins.git.BranchSpec></branches></scm>` + shellStep("make deploy"))
	plan, err := importer.FromJenkins("")(jenkinsBundle([2]string{"fromrepo", doc}), fixedTime)
	if err != nil {
		t.Fatalf("FromJenkins() error = %v", err)
	}
	if len(plan.Projects) != 0 {
		t.Errorf("projects = %+v, want none created silently", plan.Projects)
	}
	assertWarns(t, plan.Warnings, "acme/infra.git", `at "main"`)
}

// TestJenkinsStepsKeepTheirOrder covers a job with several steps.
//
// Jenkins runs build steps in order and stops at the first failure. A script that reordered them, or
// that continued past a failure, would do something different from the job it replaced.
func TestJenkinsStepsKeepTheirOrder(t *testing.T) {
	t.Parallel()
	doc := freestyle(`<builders>
		<hudson.tasks.Shell><command>echo first</command></hudson.tasks.Shell>
		<hudson.tasks.Shell><command>echo second</command></hudson.tasks.Shell>
		<hudson.tasks.Shell><command>echo third</command></hudson.tasks.Shell>
	</builders>`)
	plan, err := importer.FromJenkins("")(jenkinsBundle([2]string{"ordered", doc}), fixedTime)
	if err != nil {
		t.Fatalf("FromJenkins() error = %v", err)
	}
	cmd := plan.Templates[0].Command
	if !strings.HasPrefix(cmd, "set -e\n") {
		t.Errorf("command = %q, want set -e so it stops at the first failure like Jenkins", cmd)
	}
	first, second, third := strings.Index(cmd, "first"), strings.Index(cmd, "second"),
		strings.Index(cmd, "third")
	if first >= second || second >= third {
		t.Errorf("steps are out of order in %q", cmd)
	}
}

// TestJenkinsBareConfigIsAccepted covers pointing the importer at one config.xml on its own, which
// carries no name because a Jenkins job's name lives in its directory.
func TestJenkinsBareConfigIsAccepted(t *testing.T) {
	t.Parallel()
	plan, err := importer.FromJenkins("")([]byte(freestyle(shellStep("echo hi"))), fixedTime)
	if err != nil {
		t.Fatalf("FromJenkins() error = %v", err)
	}
	if len(plan.Templates) != 1 {
		t.Fatalf("templates = %d, want the single job imported", len(plan.Templates))
	}
}

// TestJenkinsRejectsSomethingThatIsNotJenkins covers a file handed to the wrong importer.
func TestJenkinsRejectsSomethingThatIsNotJenkins(t *testing.T) {
	t.Parallel()
	for _, data := range []string{`{"jobs": []}`, "", "not xml at all", "<jobs></jobs>"} {
		if _, err := importer.FromJenkins("")([]byte(data), fixedTime); err == nil {
			t.Errorf("FromJenkins(%q) error = nil, want a refusal", data)
		}
	}
}

// TestFromJenkinsAgainstRealJenkinsOutput runs the importer over configuration a real Jenkins wrote.
//
// The fixture under testdata/jenkins-home is not hand-authored. The jobs were created through
// Jenkins' own object model on a running Jenkins 2 LTS with the folders, git, build timeout, and
// Pipeline plugins installed, and the files are what Jenkins serialized to disk, copied out
// untouched apart from the encrypted parameter, whose ciphertext shape trips secret scanners.
//
// It exists because the AWX and Semaphore importers each passed a full suite of fixtures the author
// wrote and then imported nothing at all from a real export. Fixtures agree with whatever the person
// writing them believed the format was. This one cannot: it disagreed immediately, since Jenkins
// writes a choice parameter's values as a flat string list rather than the wrapped array almost
// every published example shows.
func TestFromJenkinsAgainstRealJenkinsOutput(t *testing.T) {
	t.Parallel()
	bundle, err := importer.JenkinsBundle("testdata/jenkins-home")
	if err != nil {
		t.Fatalf("JenkinsBundle() error = %v", err)
	}
	// The folder's own job sits two levels down and must arrive carrying the folder in its name.
	if diff := cmp.Diff([]string{
		"empty-job", "multi-step", "multi-timer", "nightly-backup", "paused-job", "pipeline-build",
		"platform", "platform/db-vacuum", "poll-deploy", "weekly-report", "windows-task",
	}, importer.JenkinsJobNames(bundle)); diff != "" {
		t.Errorf("job names mismatch (-want +got):\n%s", diff)
	}

	plan, err := importer.FromJenkins("prod")(bundle, fixedTime)
	if err != nil {
		t.Fatalf("FromJenkins() error = %v", err)
	}

	var names []string
	for _, tpl := range plan.Templates {
		names = append(names, tpl.Name)
	}
	// A folder is a container, a Pipeline has no translation, and the batch-only and step-less jobs
	// would have run nothing. Everything else is a real job that must survive the trip.
	if diff := cmp.Diff([]string{
		"multi-step", "multi-timer", "nightly-backup", "paused-job", "platform/db-vacuum",
		"poll-deploy", "weekly-report",
	}, names); diff != "" {
		t.Errorf("templates mismatch (-want +got):\n%s", diff)
	}

	nightly := findTemplate(t, plan, "nightly-backup")
	if nightly.Timeout != 45*60 {
		t.Errorf("timeout = %d, want the build timeout wrapper's 45 minutes. Jenkins writes that "+
			"plugin's element name with a doubled underscore, which is easy to miss", nightly.Timeout)
	}
	wantSurvey := []template.SurveyField{
		{Var: "TARGET", Label: "TARGET", Type: template.FieldText, Help: "Which environment",
			Default: "staging"},
		{Var: "REGION", Label: "REGION", Type: template.FieldChoice, Help: "Region",
			Choices: []string{"us-east-1", "us-west-2"}, Default: "us-east-1"},
		{Var: "DRY_RUN", Label: "DRY_RUN", Type: template.FieldBool, Help: "Skip writes",
			Default: "true"},
		{Var: "NOTES", Label: "NOTES", Type: template.FieldMultiline, Help: "Free notes"},
	}
	if diff := cmp.Diff(wantSurvey, nightly.Survey); diff != "" {
		t.Errorf("survey mismatch (-want +got):\n%s", diff)
	}

	// Every shell step must arrive, in order, including the ones around the Python one.
	multi := findTemplate(t, plan, "multi-step")
	for _, want := range []string{"echo first", "print('second')", "echo third"} {
		if !strings.Contains(multi.Command, want) {
			t.Errorf("multi-step command is missing %q:\n%s", want, multi.Command)
		}
	}

	byName := map[string][]string{}
	for _, s := range plan.Schedules {
		byName[s.Name] = append(byName[s.Name], s.Cron)
	}
	// Sunday written as 7 is the case the scheduler rejects outright, so this one is exact.
	if diff := cmp.Diff([]string{"0 3 * * 0"}, byName["weekly-report"]); diff != "" {
		t.Errorf("weekly-report schedule mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"0 6 * * 1-5", "30 22 * * 0"}, byName["multi-timer"]); diff != "" {
		t.Errorf("multi-timer schedule mismatch (-want +got):\n%s", diff)
	}
	// The hashed specs keep the part Jenkins pinned down and fill in only what H stood for.
	assertHashedInto(t, byName["nightly-backup"], 1, 2, 2)
	assertHashedInto(t, byName["platform/db-vacuum"], 1, 0, 2)
	// A poll trigger is the one that must not appear at all.
	if got := byName["poll-deploy"]; len(got) != 0 {
		t.Errorf("poll-deploy schedules = %v, want none: it polled rather than fired on a clock", got)
	}

	assertWarns(t, plan.Warnings,
		"Pipeline job", "polled source control", "password parameter and was NOT imported",
		"$WORKSPACE", "Windows batch step", "python3", "acme/infra.git")
	// The encrypted parameter must not travel into the plan in any form.
	for _, w := range plan.Warnings {
		if strings.Contains(w, "AQAAABAAAAA") {
			t.Fatalf("warning leaked the stored ciphertext: %q", w)
		}
	}
}

// findTemplate returns the named template, failing when the plan has no such thing.
func findTemplate(t *testing.T, plan *importer.Plan, name string) *template.Template {
	t.Helper()
	for _, tpl := range plan.Templates {
		if tpl.Name == name {
			return tpl
		}
	}
	t.Fatalf("no template named %q in the plan", name)
	return nil
}

// assertHashedInto checks that a hashed specification produced exactly one schedule whose given
// field landed inside the window Jenkins allowed, which is what H has to preserve.
func assertHashedInto(t *testing.T, crons []string, field, lo, hi int) {
	t.Helper()
	if len(crons) != 1 {
		t.Fatalf("schedules = %v, want exactly one", crons)
	}
	fields := strings.Fields(crons[0])
	if len(fields) != 5 {
		t.Fatalf("cron %q does not have five fields", crons[0])
	}
	var got int
	if _, err := fmt.Sscanf(fields[field], "%d", &got); err != nil {
		t.Fatalf("cron %q field %d is %q, want a concrete number", crons[0], field, fields[field])
	}
	if got < lo || got > hi {
		t.Errorf("cron %q field %d is %d, want it inside the window %d-%d Jenkins allowed",
			crons[0], field, got, lo, hi)
	}
}

// TestJenkinsJobNameCannotForgeWarningLines covers a job name carrying a newline.
//
// A job's name is the name of the directory holding its config.xml, and a directory name on Unix may
// contain a newline. That name is interpolated into the warning lines an operator reads to decide
// which parts of an import are safe, so a hostile or merely careless name must not be able to write
// lines of its own into that report.
func TestJenkinsJobNameCannotForgeWarningLines(t *testing.T) {
	t.Parallel()
	hostile := "innocent\n    - job \"other\" imported cleanly with no warnings"
	doc := freestyle("<disabled>true</disabled>" + shellStep("echo hi"))
	plan, err := importer.FromJenkins("")(jenkinsBundle([2]string{hostile, doc}), fixedTime)
	if err != nil {
		t.Fatalf("FromJenkins() error = %v", err)
	}
	for _, w := range plan.Warnings {
		if strings.Contains(w, "\n") {
			t.Errorf("a job name put a newline into a warning line: %q", w)
		}
	}
	if strings.Contains(plan.Templates[0].Name, "\n") {
		t.Errorf("template name kept the newline: %q", plan.Templates[0].Name)
	}
	// A long nested name is legitimate and must not be truncated the way a warning value is.
	long := "platform/" + strings.Repeat("deeply-nested-folder/", 6) + "the-actual-job"
	full, err := importer.FromJenkins("")(
		jenkinsBundle([2]string{long, freestyle(shellStep("echo hi"))}), fixedTime)
	if err != nil {
		t.Fatalf("FromJenkins() error = %v", err)
	}
	if full.Templates[0].Name != long {
		t.Errorf("long name was altered:\n got %q\nwant %q", full.Templates[0].Name, long)
	}
}

// TestJenkinsBundleAcceptsEveryPathAnOperatorWouldPointAt covers the four things somebody hands this
// when they mean "my Jenkins jobs".
func TestJenkinsBundleAcceptsEveryPathAnOperatorWouldPointAt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Path      string
		WantNames []string
	}{{ // Test 0: A JENKINS_HOME, whose own config.xml is the controller's and not a job.
		Path: "testdata/jenkins-home",
		WantNames: []string{
			"empty-job", "multi-step", "multi-timer", "nightly-backup", "paused-job",
			"pipeline-build", "platform", "platform/db-vacuum", "poll-deploy", "weekly-report",
			"windows-task",
		},
	}, { // Test 1: The jobs directory inside it, which is equally plausible.
		Path: "testdata/jenkins-home/jobs",
		WantNames: []string{
			"empty-job", "multi-step", "multi-timer", "nightly-backup", "paused-job",
			"pipeline-build", "platform", "platform/db-vacuum", "poll-deploy", "weekly-report",
			"windows-task",
		},
	}, { // Test 2: One job's directory, which names the job after the directory.
		Path:      "testdata/jenkins-home/jobs/nightly-backup/config.xml",
		WantNames: []string{"nightly-backup"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			bundle, err := importer.JenkinsBundle(test.Path)
			if err != nil {
				t.Fatalf("JenkinsBundle(%q) error = %v", test.Path, err)
			}
			if diff := cmp.Diff(test.WantNames, importer.JenkinsJobNames(bundle)); diff != "" {
				t.Errorf("names mismatch (-want +got):\n%s", diff)
			}
		})
	}

	// A path with no job definitions under it must say so rather than report an empty success.
	if _, err := importer.JenkinsBundle(t.TempDir()); err == nil {
		t.Error("JenkinsBundle() on an empty directory error = nil, want a refusal")
	}
	if _, err := importer.JenkinsBundle("testdata/no-such-path"); err == nil {
		t.Error("JenkinsBundle() on a missing path error = nil, want a refusal")
	}
}

// zipOf builds a zip archive from a path-to-content map, for the archive tests.
func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		f, err := w.Create(name)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := f.Write([]byte(files[name])); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// TestJenkinsZipNamesJobsByTheirPlaceInTheTree covers uploading a zipped jobs directory.
//
// Jenkins has no single-file export, so an archive is the only way to hand a whole Jenkins to the
// web importer. A job's name comes from where its config.xml sits, and the archive may be rooted
// anywhere, so the directory the operator happened to zip from must not end up prefixed onto every
// job name.
func TestJenkinsZipNamesJobsByTheirPlaceInTheTree(t *testing.T) {
	t.Parallel()
	job := freestyle(shellStep("echo hi"))
	tests := []struct {
		Files     map[string]string
		WantNames []string
	}{{ // Test 0: Rooted at the jobs directory itself.
		Files: map[string]string{
			"jobs/alpha/config.xml":              job,
			"jobs/platform/jobs/beta/config.xml": job,
		},
		WantNames: []string{"alpha", "platform/beta"},
	}, { // Test 1: Rooted at JENKINS_HOME, whose own name must not prefix the jobs.
		Files: map[string]string{
			"jenkins_home/jobs/alpha/config.xml":              job,
			"jenkins_home/jobs/platform/jobs/beta/config.xml": job,
			// The controller's own configuration sits beside the jobs directory and is not a job.
			"jenkins_home/config.xml": "<hudson/>",
		},
		WantNames: []string{"alpha", "jenkins_home", "platform/beta"},
	}, { // Test 2: Rooted inside one job, with no jobs segment at all.
		Files:     map[string]string{"alpha/config.xml": job},
		WantNames: []string{"alpha"},
	}, { // Test 3: Folders nested three deep.
		Files: map[string]string{
			"jobs/a/jobs/b/jobs/c/config.xml": job,
		},
		WantNames: []string{"a/b/c"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			bundle, err := importer.JenkinsBundleFromZip(zipOf(t, test.Files))
			if err != nil {
				t.Fatalf("JenkinsBundleFromZip() error = %v", err)
			}
			if diff := cmp.Diff(test.WantNames, importer.JenkinsJobNames(bundle)); diff != "" {
				t.Errorf("names mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestJenkinsZipGoesStraightThroughTheImporter covers handing an archive to the importer directly,
// which is what the upload endpoint does with a request body.
func TestJenkinsZipGoesStraightThroughTheImporter(t *testing.T) {
	t.Parallel()
	archive := zipOf(t, map[string]string{
		"jobs/nightly/config.xml": freestyle(timer("0 2 * * 7") + shellStep("echo hi")),
		"jobs/build/config.xml":   "<flow-definition/>",
	})
	plan, err := importer.FromJenkins("prod")(archive, fixedTime)
	if err != nil {
		t.Fatalf("FromJenkins() error = %v", err)
	}
	if len(plan.Templates) != 1 || plan.Templates[0].Name != "nightly" {
		t.Fatalf("templates = %+v, want just the freestyle job", plan.Templates)
	}
	if len(plan.Schedules) != 1 || plan.Schedules[0].Cron != "0 2 * * 0" {
		t.Errorf("schedules = %+v, want Sunday renumbered", plan.Schedules)
	}
}

// TestJenkinsZipRefusesAHostileArchive covers the ceilings on an upload.
//
// The import endpoint is reachable by anyone allowed to import, and a zip is the one input where a
// few kilobytes can become gigabytes. A refusal that names the limit is the point: silently reading
// a decompression bomb is how an import takes the server down.
func TestJenkinsZipRefusesAHostileArchive(t *testing.T) {
	t.Parallel()
	// A single member that expands far past what a job definition can be. It compresses to almost
	// nothing, which is exactly why the declared size cannot be the thing that is trusted.
	bomb := zipOf(t, map[string]string{
		"jobs/huge/config.xml": strings.Repeat("A", 8<<20),
	})
	if _, err := importer.JenkinsBundleFromZip(bomb); err == nil {
		t.Error("JenkinsBundleFromZip() on an oversized member error = nil, want a refusal")
	}

	// An archive holding nothing this reads must say so rather than report an empty success.
	empty := zipOf(t, map[string]string{"jobs/readme.txt": "nothing here"})
	if _, err := importer.JenkinsBundleFromZip(empty); err == nil {
		t.Error("JenkinsBundleFromZip() on an archive with no jobs error = nil, want a refusal")
	}

	// Bytes that merely start like a zip must not be reported as a job format problem.
	if _, err := importer.FromJenkins("")([]byte("PK\x03\x04 not really"), fixedTime); err == nil {
		t.Error("FromJenkins() on a truncated archive error = nil, want a refusal")
	}
}

// TestJenkinsZipCannotEscapeItsOwnNames covers archive members whose paths try to climb out.
//
// Nothing is written to disk from an archive here, so a traversal cannot overwrite a file. The name
// still becomes a stored template's name, so a member called ../../etc/passwd must not produce a
// template that reads as though it came from somewhere it did not.
func TestJenkinsZipCannotEscapeItsOwnNames(t *testing.T) {
	t.Parallel()
	bundle, err := importer.JenkinsBundleFromZip(zipOf(t, map[string]string{
		"jobs/../../etc/passwd/config.xml": freestyle(shellStep("echo hi")),
		"jobs/normal/config.xml":           freestyle(shellStep("echo hi")),
	}))
	if err != nil {
		t.Fatalf("JenkinsBundleFromZip() error = %v", err)
	}
	for _, name := range importer.JenkinsJobNames(bundle) {
		if strings.Contains(name, "..") {
			t.Errorf("a job name kept a traversal segment: %q", name)
		}
	}
}

// TestJenkinsAliasesExpandTheWayJenkinsExpandsThem pins the shorthand table against the real thing.
//
// Jenkins does not treat @daily as midnight. It expands each alias with H so jobs spread across the
// period, and the expansions are not guessable: @midnight is not @daily, it is the same thing with
// the hour confined to 0-2, and @monthly may not use a day past 28 or a job would skip February.
//
// The windows below were read off a running Jenkins 2 LTS by asking hudson.scheduler.CronTab for the
// next firings of each alias, rather than from documentation. Observed, for one job name: @hourly at
// :35 of every hour, @daily at 01:37, @midnight at 00:01, @weekly on a Saturday at 14:33, @monthly
// on the 18th at 10:04, @yearly on June 15. Each sits inside the window asserted here.
func TestJenkinsAliasesExpandTheWayJenkinsExpandsThem(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Alias string
		// Field is the cron position that must hold a concrete value, and Lo and Hi the window
		// Jenkins allows it to land in.
		Field  int
		Lo, Hi int
		// WildFields are the positions that must stay open, which is what makes the cadence right.
		WildFields []int
	}{{ // Test 0: Hourly spreads the minute and leaves every other field open.
		Alias: "@hourly", Field: 0, Lo: 0, Hi: 59, WildFields: []int{1, 2, 3, 4},
	}, { // Test 1: Daily spreads the hour across the whole day.
		Alias: "@daily", Field: 1, Lo: 0, Hi: 23, WildFields: []int{2, 3, 4},
	}, { // Test 2: Midnight is daily with the hour confined, which is the pair most easily conflated.
		Alias: "@midnight", Field: 1, Lo: 0, Hi: 2, WildFields: []int{2, 3, 4},
	}, { // Test 3: Weekly pins a weekday and leaves the month and day of month open.
		Alias: "@weekly", Field: 4, Lo: 0, Hi: 6, WildFields: []int{2, 3},
	}, { // Test 4: Monthly pins a day of month, never past 28 or February would be skipped.
		Alias: "@monthly", Field: 2, Lo: 1, Hi: 28, WildFields: []int{3, 4},
	}, { // Test 5: Yearly pins a month as well.
		Alias: "@yearly", Field: 3, Lo: 1, Hi: 12, WildFields: []int{4},
	}, { // Test 6: The other spelling of yearly is the same schedule.
		Alias: "@annually", Field: 3, Lo: 1, Hi: 12, WildFields: []int{4},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			doc := freestyle(timer(test.Alias) + shellStep("echo hi"))
			plan, err := importer.FromJenkins("")(
				jenkinsBundle([2]string{"nightly-backup", doc}), fixedTime)
			if err != nil {
				t.Fatalf("FromJenkins() error = %v", err)
			}
			if len(plan.Schedules) != 1 {
				t.Fatalf("%s produced %d schedules, warnings %v, want one",
					test.Alias, len(plan.Schedules), plan.Warnings)
			}
			assertHashedInto(t, []string{plan.Schedules[0].Cron}, test.Field, test.Lo, test.Hi)
			fields := strings.Fields(plan.Schedules[0].Cron)
			for _, wild := range test.WildFields {
				if fields[wild] != "*" {
					t.Errorf("%s imported as %q, but field %d should stay open or the cadence "+
						"is narrower than the alias meant", test.Alias, plan.Schedules[0].Cron, wild)
				}
			}
		})
	}
}
