package importer

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// jenkinsFreestyle wraps a body in a freestyle job document, so each case below carries only the
// part it is about. It deliberately carries no XML declaration, because a real Jenkins writes
// version 1.1 and that is refused on this path, which is pinned on its own below.
func jenkinsFreestyle(body string) string {
	return `<project>` + body + `</project>`
}

// jenkinsPlan runs one Jenkins document through the importer and fails the test if it does not
// parse.
func jenkinsPlan(t *testing.T, inventory, doc string) *Plan {
	t.Helper()
	plan, err := FromJenkins(inventory)([]byte(doc), importNow)
	if err != nil {
		t.Fatalf("FromJenkins() error = %v\ndocument:\n%s", err, doc)
	}
	return plan
}

// TestJenkinsRefusesWhatIsNotAJenkinsDocument pins the parse failures. A file that is not Jenkins
// must be refused rather than turned into an empty plan an operator reads as success.
func TestJenkinsRefusesWhatIsNotAJenkinsDocument(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Doc  string
	}{
		{Name: "empty", Doc: ""},                                 // Test 0.
		{Name: "not xml", Doc: "just text"},                      // Test 1.
		{Name: "unclosed", Doc: "<project><builders>"},           // Test 2.
		{Name: "declaration only", Doc: "<?xml version='1.1'?>"}, // Test 3.
		{Name: "empty bundle", Doc: "<jobs></jobs>"},             // Test 4.
		{Name: "json", Doc: `{"jobs": []}`},                      // Test 5.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			plan, err := FromJenkins("prod")([]byte(test.Doc), importNow)
			if err == nil {
				t.Fatalf("FromJenkins() error = nil, want a refusal; plan = %+v", plan)
			}
		})
	}
}

// TestJenkinsRefusesEveryJobTypeItCannotTranslateByName pins that a job with no faithful equivalent
// is named and skipped rather than half-imported into something that would not do what it used to.
// A Pipeline job is a Groovy program, and there is no honest mechanical translation to a template.
func TestJenkinsRefusesEveryJobTypeItCannotTranslateByName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		Root         string
		WantFragment string
	}{
		{Name: "pipeline", Root: "flow-definition",
			WantFragment: "a Pipeline job"}, // Test 0.
		{Name: "multibranch",
			Root:         "org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject",
			WantFragment: "a multibranch"}, // Test 1.
		{Name: "matrix", Root: "matrix-project", WantFragment: "a matrix job"},     // Test 2.
		{Name: "maven", Root: "maven2-moduleset", WantFragment: "a Maven job"},     // Test 3.
		{Name: "ivy", Root: "hudson.ivy.IvyModuleSet", WantFragment: "an Ivy job"}, // Test 4.
		{Name: "external", Root: "hudson.model.ExternalJob",
			WantFragment: "an external job"}, // Test 5.
		{Name: "unknown", Root: "com.example.SomethingElse",
			WantFragment: "unrecognized type"}, // Test 6.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := fmt.Sprintf(`<jobs><job name="ops/thing"><%s></%s></job>
				<job name="ok"><project><builders>
				<hudson.tasks.Shell><command>echo hi</command></hudson.tasks.Shell>
				</builders></project></job></jobs>`, test.Root, test.Root)
			plan := jenkinsPlan(t, "prod", doc)
			if len(plan.Templates) != 1 || plan.Templates[0].Name != "ok" {
				t.Fatalf("templates = %+v, want only the freestyle job", plan.Templates)
			}
			if _, ok := warningContaining(t, plan.Warnings, `job "ops/thing"`,
				test.WantFragment); !ok {
				t.Errorf("missing %q.\nwarnings: %v", test.WantFragment, plan.Warnings)
			}
		})
	}
}

// TestJenkinsFolderIsSkippedSilently pins that a folder produces no warning and no template. The
// walker already flattened its contents into the names of the jobs inside it, so reporting the
// folder would be noise in a report an operator has to read line by line.
func TestJenkinsFolderIsSkippedSilently(t *testing.T) {
	t.Parallel()
	const doc = `<jobs>
      <job name="ops"><com.cloudbees.hudson.plugins.folder.Folder/></job>
      <job name="ops/build"><project><builders>
        <hudson.tasks.Shell><command>echo hi</command></hudson.tasks.Shell>
      </builders></project></job>
    </jobs>`
	plan := jenkinsPlan(t, "prod", doc)
	if len(plan.Templates) != 1 || plan.Templates[0].Name != "ops/build" {
		t.Fatalf("templates = %+v, want only the job inside the folder", plan.Templates)
	}
	for _, w := range plan.Warnings {
		if strings.Contains(w, `job "ops"`) {
			t.Errorf("the folder itself was reported: %s", w)
		}
	}
}

// TestJenkinsCleanNameFoldsControlCharactersWithoutClipping pins the name sanitizer. A job's name
// comes from a directory name, which on Unix may hold a newline, and it is interpolated into warning
// lines an operator reads to decide what is safe. Clipping is deliberately not applied, because a
// deeply nested job has a long legitimate name and this value becomes the template's real name.
func TestJenkinsCleanNameFoldsControlCharactersWithoutClipping(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a/", 100) + "job"
	tests := []struct {
		Name       string
		In         string
		WantResult string
	}{
		{Name: "plain", In: "ops/build", WantResult: "ops/build"},                       // Test 0.
		{Name: "spaces kept", In: "My Build Job", WantResult: "My Build Job"},           // Test 1.
		{Name: "newline folded", In: "a\nWARNING: fake", WantResult: "a WARNING: fake"}, // Test 2.
		{Name: "carriage folded", In: "a\rb", WantResult: "a b"},                        // Test 3.
		{Name: "tab folded", In: "a\tb", WantResult: "a b"},                             // Test 4.
		{Name: "nul folded", In: "a\x00b", WantResult: "a b"},                           // Test 5.
		{Name: "delete folded", In: "a\x7fb", WantResult: "a b"},                        // Test 6.
		{Name: "runs collapsed", In: "a  \n\t  b", WantResult: "a b"},                   // Test 7.
		{Name: "trimmed", In: "  a  ", WantResult: "a"},                                 // Test 8.
		{Name: "empty", In: "", WantResult: ""},                                         // Test 9.
		{Name: "only control", In: "\n\t", WantResult: ""},                              // Test 10.
		{Name: "unicode kept", In: "ビルド/生产", WantResult: "ビルド/生产"},                      // Test 11.
		{Name: "long name is not clipped", In: long, WantResult: long},                  // Test 12.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := jenkinsCleanName(test.In); got != test.WantResult {
				t.Errorf("jenkinsCleanName(%q) = %q, want %q", test.In, got, test.WantResult)
			}
		})
	}
}

// TestJenkinsUnnamedJobFallsBackToItsRootElement pins that a lone config.xml handed over on its own,
// which carries no name of its own, still produces a named template. A template with an empty name
// is one nobody can find again.
func TestJenkinsUnnamedJobFallsBackToItsRootElement(t *testing.T) {
	t.Parallel()
	doc := jenkinsFreestyle(`<builders>
      <hudson.tasks.Shell><command>echo hi</command></hudson.tasks.Shell>
    </builders>`)
	plan := jenkinsPlan(t, "prod", doc)
	if len(plan.Templates) != 1 || plan.Templates[0].Name != "project" {
		t.Fatalf("templates = %+v, want one named after its root element", plan.Templates)
	}
}

// TestJenkinsParameterKindsMapOrAreRefused pins each parameter definition. A password parameter is
// stored encrypted by Jenkins while a survey answer is kept in the clear on every run, and a file
// upload and a build picker have no equivalent at all, so each is dropped and named rather than
// turned into free text that would not do what it used to.
//
//nolint:funlen // Test function.
func TestJenkinsParameterKindsMapOrAreRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		Param        string
		WantType     string
		WantDefault  any
		WantChoices  []string
		WantImported bool
		WantWarning  string
	}{
		{Name: "string", Param: `<hudson.model.StringParameterDefinition><name>env</name>
			<defaultValue>prod</defaultValue></hudson.model.StringParameterDefinition>`,
			WantType: "text", WantDefault: "prod", WantImported: true}, // Test 0.
		{Name: "string with no default", Param: `<hudson.model.StringParameterDefinition>
			<name>env</name><defaultValue></defaultValue>
			</hudson.model.StringParameterDefinition>`,
			WantType: "text", WantDefault: nil, WantImported: true}, // Test 1: blank is no default,
		// not a default of the empty string.
		{Name: "boolean", Param: `<hudson.model.BooleanParameterDefinition><name>force</name>
			<defaultValue>true</defaultValue></hudson.model.BooleanParameterDefinition>`,
			WantType: "bool", WantDefault: "true", WantImported: true}, // Test 2.
		{Name: "text becomes multiline", Param: `<hudson.model.TextParameterDefinition>
			<name>notes</name></hudson.model.TextParameterDefinition>`,
			WantType: "multiline", WantImported: true}, // Test 3.
		{Name: "nested choices", Param: `<hudson.model.ChoiceParameterDefinition><name>env</name>
			<choices><a><string>prod</string><string>stage</string></a></choices>
			</hudson.model.ChoiceParameterDefinition>`,
			WantType: "choice", WantChoices: []string{"prod", "stage"}, WantDefault: "prod",
			WantImported: true}, // Test 4: the first entry is what Jenkins offers.
		{Name: "flat choices", Param: `<hudson.model.ChoiceParameterDefinition><name>env</name>
			<choices><string>a</string><string>b</string></choices>
			</hudson.model.ChoiceParameterDefinition>`,
			WantType: "choice", WantChoices: []string{"a", "b"}, WantDefault: "a",
			WantImported: true}, // Test 5: the older unwrapped form.
		{Name: "choice with no values", Param: `<hudson.model.ChoiceParameterDefinition>
			<name>env</name></hudson.model.ChoiceParameterDefinition>`,
			WantType: "text", WantImported: true,
			WantWarning: "is a choice with no values"}, // Test 6.
		{Name: "unknown kind", Param: `<com.example.WeirdParameterDefinition><name>env</name>
			</com.example.WeirdParameterDefinition>`,
			WantType: "text", WantImported: true,
			WantWarning: "imports as free text"}, // Test 7.
		{Name: "password refused", Param: `<hudson.model.PasswordParameterDefinition>
			<name>token</name></hudson.model.PasswordParameterDefinition>`,
			WantWarning: "is a password parameter and was NOT imported"}, // Test 8.
		{Name: "masked password refused", Param: `<com.michelin.cio.hudson.plugins.maskedpassword.` +
			`MaskedPasswordParameterDefinition><name>token</name>
			</com.michelin.cio.hudson.plugins.maskedpassword.MaskedPasswordParameterDefinition>`,
			WantWarning: "is a password parameter and was NOT imported"}, // Test 9.
		{Name: "file refused", Param: `<hudson.model.FileParameterDefinition><name>blob</name>
			</hudson.model.FileParameterDefinition>`,
			WantWarning: "uploads a file at launch"}, // Test 10.
		{Name: "run picker refused", Param: `<hudson.model.RunParameterDefinition><name>build</name>
			</hudson.model.RunParameterDefinition>`,
			WantWarning: "picks a previous build"}, // Test 11.
		{Name: "no name skipped", Param: `<hudson.model.StringParameterDefinition>
			</hudson.model.StringParameterDefinition>`,
			WantWarning: "has a parameter with no name"}, // Test 12.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := jenkinsFreestyle(`<properties>
				<hudson.model.ParametersDefinitionProperty><parameterDefinitions>` +
				test.Param + `</parameterDefinitions></hudson.model.ParametersDefinitionProperty>
				</properties><builders>
				<hudson.tasks.Shell><command>echo hi</command></hudson.tasks.Shell></builders>`)
			plan := jenkinsPlan(t, "prod", doc)
			survey := plan.Templates[0].Survey
			if got := len(survey) == 1; got != test.WantImported {
				t.Fatalf("survey = %+v, want imported = %v.\nwarnings: %v",
					survey, test.WantImported, plan.Warnings)
			}
			if test.WantImported {
				if string(survey[0].Type) != test.WantType {
					t.Errorf("type = %q, want %q", survey[0].Type, test.WantType)
				}
				if diff := cmp.Diff(test.WantDefault, survey[0].Default); diff != "" {
					t.Errorf("default mismatch (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff(test.WantChoices, survey[0].Choices,
					cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("choices mismatch (-want +got):\n%s", diff)
				}
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

// TestJenkinsBuildStepsAreMappedOrNamed pins which build steps become script and which are reported.
// A step guessed at would run something the operator did not write, and a job made only of
// unmappable steps is skipped rather than imported as a template that goes green doing nothing.
func TestJenkinsBuildStepsAreMappedOrNamed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		Builders     string
		WantImported bool
		WantWarning  string
	}{
		{Name: "shell", Builders: `<hudson.tasks.Shell><command>echo hi</command>
			</hudson.tasks.Shell>`, WantImported: true}, // Test 0.
		{Name: "empty shell", Builders: `<hudson.tasks.Shell><command>  </command>
			</hudson.tasks.Shell>`, WantWarning: "is an empty shell step"}, // Test 1.
		{Name: "batch", Builders: `<hudson.tasks.BatchFile><command>dir</command>
			</hudson.tasks.BatchFile>`,
			WantWarning: "is a Windows batch step"}, // Test 2.
		{Name: "ant", Builders: `<hudson.tasks.Ant><targets>clean build</targets>
			</hudson.tasks.Ant>`,
			WantWarning: `Ant step running "clean build"`}, // Test 3.
		{Name: "maven", Builders: `<hudson.tasks.Maven><targets>package</targets>
			</hudson.tasks.Maven>`, WantWarning: "Maven step"}, // Test 4.
		{Name: "gradle", Builders: `<hudson.plugins.gradle.Gradle/>`,
			WantWarning: "Gradle step"}, // Test 5.
		{Name: "copy artifact", Builders: `<hudson.plugins.copyartifact.CopyArtifact/>`,
			WantWarning: "copy artifact step"}, // Test 6.
		{Name: "junit", Builders: `<hudson.tasks.junit.JUnitResultArchiver/>`,
			WantWarning: "JUnit step"}, // Test 7.
		{Name: "unknown plugin", Builders: `<com.example.Weird__Step/>`,
			WantWarning: `is a "com.example.Weird_Step" step`}, // Test 8: XStream doubles an
		// underscore in a class name, so one reads oddly in a message.
		{Name: "no builders at all", Builders: "",
			WantWarning: "none of its build steps could be imported"}, // Test 9.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			// A second, ordinary job rides along so a case that imports nothing still leaves the
			// plan with an object in it and reaches the warnings rather than the empty refusal.
			doc := `<jobs><job name="subject"><project><builders>` + test.Builders +
				`</builders></project></job><job name="other"><project><builders>` +
				`<hudson.tasks.Shell><command>echo hi</command></hudson.tasks.Shell>` +
				`</builders></project></job></jobs>`
			plan := jenkinsPlan(t, "prod", doc)
			if got := len(plan.Templates) == 2; got != test.WantImported {
				t.Fatalf("templates = %+v, want the subject imported = %v.\nwarnings: %v",
					plan.Templates, test.WantImported, plan.Warnings)
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

// TestJenkinsShebangIsReportedOnlyWhenItIsNotAShell pins the interpreter check. Reading the last
// word once called "#!/bin/bash -xe" a non-shell script named "-xe" and reported every ordinary
// shell step in the export, which is the kind of noise that makes an operator stop reading.
func TestJenkinsShebangIsReportedOnlyWhenItIsNotAShell(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Interp      string
		WantIsShell bool
	}{
		{Interp: "/bin/sh", WantIsShell: true},                 // Test 0.
		{Interp: "/bin/bash", WantIsShell: true},               // Test 1.
		{Interp: "/bin/bash -xe", WantIsShell: true},           // Test 2.
		{Interp: "/usr/bin/env bash", WantIsShell: true},       // Test 3.
		{Interp: "/usr/bin/env -S bash -e", WantIsShell: true}, // Test 4.
		{Interp: "/usr/bin/env FOO=1 zsh", WantIsShell: true},  // Test 5.
		{Interp: "dash", WantIsShell: true},                    // Test 6.
		{Interp: "/bin/ksh", WantIsShell: true},                // Test 7.
		{Interp: "/bin/ash", WantIsShell: true},                // Test 8.
		{Interp: "/usr/bin/python3", WantIsShell: false},       // Test 9.
		{Interp: "/usr/bin/env python3", WantIsShell: false},   // Test 10.
		{Interp: "/usr/bin/env", WantIsShell: false},           // Test 11: env names nothing.
		{Interp: "/usr/bin/env -S", WantIsShell: false},        // Test 12.
		{Interp: "", WantIsShell: false},                       // Test 13.
		{Interp: "   ", WantIsShell: false},                    // Test 14.
		{Interp: "/usr/bin/perl", WantIsShell: false},          // Test 15.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Interp), func(t *testing.T) {
			t.Parallel()
			if got := jenkinsShellShebang(test.Interp); got != test.WantIsShell {
				t.Errorf("jenkinsShellShebang(%q) = %v, want %v",
					test.Interp, got, test.WantIsShell)
			}
		})
	}
}

// TestJenkinsTimeoutIsReadFromTheWrapperOrReported pins the build timeout conversion. A timeout read
// wrong is a job killed early or never killed at all, and the wrapper writes its cap in minutes.
func TestJenkinsTimeoutIsReadFromTheWrapperOrReported(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Wrappers    string
		WantSeconds int
		WantWarning bool
	}{
		{Name: "none", Wrappers: "", WantSeconds: 0}, // Test 0.
		{Name: "ten minutes", Wrappers: `<w><strategy><timeoutMinutes>10</timeoutMinutes>
			</strategy></w>`, WantSeconds: 600}, // Test 1.
		{Name: "one minute", Wrappers: `<w><strategy><timeoutMinutes>1</timeoutMinutes>
			</strategy></w>`, WantSeconds: 60}, // Test 2.
		{Name: "spaced", Wrappers: `<w><strategy><timeoutMinutes> 5 </timeoutMinutes>
			</strategy></w>`, WantSeconds: 300}, // Test 3.
		{Name: "zero refused", Wrappers: `<w><strategy><timeoutMinutes>0</timeoutMinutes>
			</strategy></w>`, WantSeconds: 0, WantWarning: true}, // Test 4.
		{Name: "negative refused", Wrappers: `<w><strategy><timeoutMinutes>-5</timeoutMinutes>
			</strategy></w>`, WantSeconds: 0, WantWarning: true}, // Test 5.
		{Name: "expression refused", Wrappers: `<w><strategy>
			<timeoutMinutes>${TIMEOUT}</timeoutMinutes></strategy></w>`,
			WantSeconds: 0, WantWarning: true}, // Test 6.
		{Name: "blank wrapper skipped", Wrappers: `<a/><w><strategy>
			<timeoutMinutes>3</timeoutMinutes></strategy></w>`, WantSeconds: 180}, // Test 7.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := jenkinsFreestyle(`<buildWrappers>` + test.Wrappers + `</buildWrappers>
				<builders><hudson.tasks.Shell><command>echo hi</command></hudson.tasks.Shell>
				</builders>`)
			plan := jenkinsPlan(t, "prod", doc)
			if got := plan.Templates[0].Timeout; got != test.WantSeconds {
				t.Errorf("Timeout = %d, want %d", got, test.WantSeconds)
			}
			_, warned := warningContaining(t, plan.Warnings, "build timeout")
			if warned != test.WantWarning {
				t.Errorf("timeout warning = %v, want %v.\nwarnings: %v",
					warned, test.WantWarning, plan.Warnings)
			}
		})
	}
}

// TestJenkinsTriggersBecomeSchedulesOrAreRefused pins that only a timer becomes a schedule. A poll
// trigger looks like a schedule and is not one: it asks the repository whether anything changed and
// builds only if something did, so importing it as a plain schedule would turn a job that usually
// does nothing into one that runs every few minutes unconditionally.
func TestJenkinsTriggersBecomeSchedulesOrAreRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name          string
		Trigger       string
		WantSchedules int
		WantWarning   string
	}{
		{Name: "timer", Trigger: `<hudson.triggers.TimerTrigger><spec>0 2 * * *</spec>
			</hudson.triggers.TimerTrigger>`, WantSchedules: 1}, // Test 0.
		{Name: "poll refused", Trigger: `<hudson.triggers.SCMTrigger><spec>H/5 * * * *</spec>
			</hudson.triggers.SCMTrigger>`,
			WantWarning: "That is not a schedule, so it was NOT imported"}, // Test 1.
		{Name: "reverse build refused", Trigger: `<hudson.triggers.ReverseBuildTrigger/>`,
			WantWarning: "ran after another job finished"}, // Test 2.
		{Name: "unknown trigger", Trigger: `<com.example.WeirdTrigger/>`,
			WantWarning: "trigger, which has no equivalent"}, // Test 3.
		{Name: "comments and blanks", Trigger: `<hudson.triggers.TimerTrigger>
			<spec># nightly&#10;&#10;0 2 * * *&#10;0 14 * * * # afternoon</spec>
			</hudson.triggers.TimerTrigger>`, WantSchedules: 2}, // Test 4.
		{Name: "unsupported macro", Trigger: `<hudson.triggers.TimerTrigger>
			<spec>@reboot</spec></hudson.triggers.TimerTrigger>`,
			WantWarning: "not a schedule this can express"}, // Test 5.
		{Name: "wrong field count", Trigger: `<hudson.triggers.TimerTrigger>
			<spec>0 2 * *</spec></hudson.triggers.TimerTrigger>`,
			WantWarning: "not a five field cron expression"}, // Test 6.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := jenkinsFreestyle(`<triggers>` + test.Trigger + `</triggers>
				<builders><hudson.tasks.Shell><command>echo hi</command></hudson.tasks.Shell>
				</builders>`)
			plan := jenkinsPlan(t, "prod", doc)
			if len(plan.Schedules) != test.WantSchedules {
				t.Fatalf("schedules = %d, want %d.\nwarnings: %v",
					len(plan.Schedules), test.WantSchedules, plan.Warnings)
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

// TestJenkinsHashTermsAreRefusedRatherThanGuessed pins the malformed H forms. A term this cannot
// read must drop the schedule rather than resolve to some number, because a guessed cadence fires at
// a time nobody chose and looks exactly like a converted one.
func TestJenkinsHashTermsAreRefusedRatherThanGuessed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Spec        string
		WantOK      bool
		WantWarning string
	}{
		{Name: "bare hash", Spec: "H H * * *", WantOK: true},                // Test 0.
		{Name: "windowed", Spec: "H(0-29) 2 * * *", WantOK: true},           // Test 1.
		{Name: "single point window", Spec: "H(5-5) 2 * * *", WantOK: true}, // Test 2.
		{Name: "step", Spec: "H/15 * * * *", WantOK: true},                  // Test 3.
		{Name: "windowed step", Spec: "H(0-29)/10 * * * *", WantOK: true},   // Test 4.
		{Name: "step wider than the window", Spec: "H/90 * * * *",
			WantOK: true}, // Test 5: it fires once, which a range with a step cannot express.
		{Name: "missing bracket", Spec: "H(0-29 2 * * *",
			WantWarning: "missing a closing bracket"}, // Test 6.
		{Name: "backwards range", Spec: "H(29-0) 2 * * *",
			WantWarning: "is not valid for that field"}, // Test 7.
		{Name: "range past the field", Spec: "H(0-99) 2 * * *",
			WantWarning: "is not valid for that field"}, // Test 8.
		{Name: "range below the field", Spec: "* * H(0-5) * *",
			WantWarning: "is not valid for that field"}, // Test 9: the day of month starts at one.
		{Name: "range without a dash", Spec: "H(5) 2 * * *",
			WantWarning: "is not valid for that field"}, // Test 10.
		{Name: "non numeric range", Spec: "H(a-b) 2 * * *",
			WantWarning: "is not valid for that field"}, // Test 11.
		{Name: "trailing junk", Spec: "Hx 2 * * *", WantWarning: "could not be read"}, // Test 12.
		{Name: "empty step", Spec: "H/ * * * *", WantWarning: "step in"},              // Test 13.
		{Name: "zero step", Spec: "H/0 * * * *", WantWarning: "step in"},              // Test 14.
		{Name: "negative step", Spec: "H/-5 * * * *", WantWarning: "step in"},         // Test 15.
		{Name: "non numeric step", Spec: "H/x * * * *", WantWarning: "step in"},       // Test 16.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := jenkinsFreestyle(`<triggers><hudson.triggers.TimerTrigger><spec>` +
				test.Spec + `</spec></hudson.triggers.TimerTrigger></triggers>
				<builders><hudson.tasks.Shell><command>echo hi</command></hudson.tasks.Shell>
				</builders>`)
			plan := jenkinsPlan(t, "prod", doc)
			if got := len(plan.Schedules) == 1; got != test.WantOK {
				t.Fatalf("spec %q imported = %v, want %v.\nwarnings: %v",
					test.Spec, got, test.WantOK, plan.Warnings)
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

// TestJenkinsRangeParsesOnlyAWellFormedWindow pins the small parser behind H(a-b). A window read
// wrong lands the job outside the hours the operator picked for it.
func TestJenkinsRangeParsesOnlyAWellFormedWindow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In       string
		WantLow  int
		WantHigh int
		WantOK   bool
	}{
		{In: "0-29", WantLow: 0, WantHigh: 29, WantOK: true},   // Test 0.
		{In: " 1 - 5 ", WantLow: 1, WantHigh: 5, WantOK: true}, // Test 1.
		{In: "5-5", WantLow: 5, WantHigh: 5, WantOK: true},     // Test 2.
		{In: "", WantOK: false},                                // Test 3.
		{In: "5", WantOK: false},                               // Test 4.
		{In: "a-b", WantOK: false},                             // Test 5.
		{In: "1-b", WantOK: false},                             // Test 6.
		{In: "1-2-3", WantOK: false},                           // Test 7.
		{In: "-1-5", WantOK: false},                            // Test 8: the leading dash splits
		// first, leaving an empty low bound.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %q", testNum, test.In), func(t *testing.T) {
			t.Parallel()
			low, high, ok := jenkinsRange(test.In)
			if ok != test.WantOK {
				t.Fatalf("jenkinsRange(%q) ok = %v, want %v", test.In, ok, test.WantOK)
			}
			if ok && (low != test.WantLow || high != test.WantHigh) {
				t.Errorf("jenkinsRange(%q) = %d, %d, want %d, %d",
					test.In, low, high, test.WantLow, test.WantHigh)
			}
		})
	}
}

// TestJenkinsWeekdayRenumbersOnlyTheWeekdayField pins the Sunday rewrite. Jenkins numbers Sunday as
// both 0 and 7 while the scheduler accepts only 0, and rewriting the digit in place made "1-7" into
// "1-0", a range that runs backwards, so a job that ran every day of the week simply stopped
// existing.
func TestJenkinsWeekdayRenumbersOnlyTheWeekdayField(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Term       string
		Index      int
		WantResult string
	}{
		{Name: "seven becomes zero", Term: "7", Index: 4, WantResult: "0"},     // Test 0.
		{Name: "other fields untouched", Term: "7", Index: 0, WantResult: "7"}, // Test 1.
		{Name: "plain day", Term: "3", Index: 4, WantResult: "3"},              // Test 2.
		{Name: "full week range split", Term: "1-7", Index: 4,
			WantResult: "1,2,3,4,5,6,0"}, // Test 3.
		{Name: "sunday to sunday", Term: "7-7", Index: 4, WantResult: "0"},             // Test 4.
		{Name: "range not reaching sunday", Term: "1-5", Index: 4, WantResult: "1-5"},  // Test 5.
		{Name: "names pass through", Term: "MON-FRI", Index: 4, WantResult: "MON-FRI"}, // Test 6.
		{Name: "stepped range", Term: "1-7/2", Index: 4, WantResult: "1,3,5,0"},        // Test 7.
		{Name: "stepped seven alone", Term: "7/2", Index: 4, WantResult: "7/2"},        // Test 8.
		{Name: "bad step", Term: "1-7/x", Index: 4, WantResult: "1-7/x"},               // Test 9.
		{Name: "zero step", Term: "1-7/0", Index: 4, WantResult: "1-7/0"},              // Test 10.
		{Name: "star", Term: "*", Index: 4, WantResult: "*"},                           // Test 11.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := jenkinsWeekday(test.Term, test.Index); got != test.WantResult {
				t.Errorf("jenkinsWeekday(%q, %d) = %q, want %q",
					test.Term, test.Index, got, test.WantResult)
			}
		})
	}
}

// TestJenkinsHashIsStableAndFieldDependent pins the two properties H has to keep. A re-import must
// produce the identical schedule rather than moving the job every time, and a spec whose fields are
// all H must not put the same number in each.
func TestJenkinsHashIsStableAndFieldDependent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Job  string
	}{
		{Name: "plain", Job: "build"},                             // Test 0.
		{Name: "empty", Job: ""},                                  // Test 1.
		{Name: "unicode", Job: "ビルド"},                             // Test 2.
		{Name: "long", Job: strings.Repeat("deep/", 100) + "job"}, // Test 3: the fold must not
		// overflow into a negative modulus, which would index outside the field.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			for field := range 5 {
				first, second := jenkinsHash(test.Job, field), jenkinsHash(test.Job, field)
				if first != second {
					t.Fatalf("jenkinsHash(%q, %d) is not stable: %d then %d",
						test.Job, field, first, second)
				}
				if first < 0 {
					t.Errorf("jenkinsHash(%q, %d) = %d, want a non-negative value",
						test.Job, field, first)
				}
			}
		})
	}
}

// TestJenkinsSCMIsNamedRatherThanAttached pins that a job's repository is reported and not wired up.
// A freestyle job checks a repository out into its workspace and runs its steps there, so attaching
// a project silently would change where every relative path in the script resolves.
func TestJenkinsSCMIsNamedRatherThanAttached(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		SCM          string
		WantWarning  bool
		WantFragment string
	}{
		{Name: "no scm", SCM: "<scm/>"}, // Test 0.
		{Name: "url only", SCM: `<scm><userRemoteConfigs>
			<hudson.plugins.git.UserRemoteConfig><url>https://git.example/i.git</url>
			</hudson.plugins.git.UserRemoteConfig></userRemoteConfigs></scm>`,
			WantWarning: true, WantFragment: "https://git.example/i.git"}, // Test 1.
		{Name: "branch spec trimmed", SCM: `<scm><userRemoteConfigs>
			<hudson.plugins.git.UserRemoteConfig><url>https://git.example/i.git</url>
			</hudson.plugins.git.UserRemoteConfig></userRemoteConfigs>
			<branches><hudson.plugins.git.BranchSpec><name>*/main</name>
			</hudson.plugins.git.BranchSpec></branches></scm>`,
			WantWarning: true, WantFragment: `at "main"`}, // Test 2.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := jenkinsFreestyle(test.SCM + `<builders>
				<hudson.tasks.Shell><command>echo hi</command></hudson.tasks.Shell></builders>`)
			plan := jenkinsPlan(t, "prod", doc)
			if plan.Templates[0].ProjectID != "" {
				t.Errorf("ProjectID = %q, want empty: a repository is never attached for the "+
					"operator", plan.Templates[0].ProjectID)
			}
			_, warned := warningContaining(t, plan.Warnings, "checked out")
			if warned != test.WantWarning {
				t.Fatalf("scm warning = %v, want %v.\nwarnings: %v",
					warned, test.WantWarning, plan.Warnings)
			}
			if test.WantFragment == "" {
				return
			}
			if _, ok := warningContaining(t, plan.Warnings, test.WantFragment); !ok {
				t.Errorf("missing %q.\nwarnings: %v", test.WantFragment, plan.Warnings)
			}
		})
	}
}

// TestJenkinsFoundLineDescribesTheWalk pins the line printed before the plan, so an import that
// skips most of a Jenkins says what it found first rather than only reporting a small plan.
func TestJenkinsFoundLineDescribesTheWalk(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Names      []string
		WantResult string
	}{
		{Name: "none", Names: nil, WantResult: ""},               // Test 0.
		{Name: "empty slice", Names: []string{}, WantResult: ""}, // Test 1.
		{Name: "one", Names: []string{"build"},
			WantResult: "Found 1 job definition(s): build"}, // Test 2.
		{Name: "several", Names: []string{"a", "ops/b"},
			WantResult: "Found 2 job definition(s): a, ops/b"}, // Test 3.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := JenkinsFoundLine(test.Names); got != test.WantResult {
				t.Errorf("JenkinsFoundLine(%v) = %q, want %q", test.Names, got, test.WantResult)
			}
		})
	}
}

// TestJenkinsJobNamesReadsABundleAndRefusesRubbish pins the name lister. It is read from an
// already-built bundle, so a document that does not parse must produce no names rather than a panic
// or a partial list the operator would take as the whole export.
func TestJenkinsJobNamesReadsABundleAndRefusesRubbish(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		Bundle    string
		WantNames []string
	}{
		{Name: "two jobs", Bundle: `<jobs><job name="a"><project/></job>
			<job name="ops/b"><project/></job></jobs>`,
			WantNames: []string{"a", "ops/b"}}, // Test 0.
		{Name: "empty bundle", Bundle: `<jobs></jobs>`, WantNames: nil}, // Test 1.
		{Name: "not xml", Bundle: `not xml`, WantNames: nil},            // Test 2.
		{Name: "unnamed job", Bundle: `<jobs><job><project/></job></jobs>`,
			WantNames: []string{""}}, // Test 3.
		{Name: "escaped name", Bundle: `<jobs><job name="a&quot;b"><project/></job></jobs>`,
			WantNames: []string{`a"b`}}, // Test 4.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := JenkinsJobNames([]byte(test.Bundle))
			if diff := cmp.Diff(test.WantNames, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("JenkinsJobNames() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestStripXMLDeclarationOnlyRemovesALeadingOne pins the trim that lets a config.xml be nested
// inside a bundle. A declaration is legal only at the very start of a document, and removing more
// than that would eat the job's own first element.
func TestStripXMLDeclarationOnlyRemovesALeadingOne(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		In         string
		WantResult string
	}{
		{Name: "declaration", In: `<?xml version='1.1'?><project/>`,
			WantResult: `<project/>`}, // Test 0.
		{Name: "leading whitespace", In: "  \n<?xml version='1.1'?>\n<project/>",
			WantResult: `<project/>`}, // Test 1.
		{Name: "byte order mark", In: "\uFEFF<?xml version='1.1'?><project/>",
			WantResult: `<project/>`}, // Test 2.
		{Name: "no declaration", In: `<project/>`, WantResult: `<project/>`}, // Test 3.
		{Name: "unterminated declaration", In: `<?xml version='1.1'`,
			WantResult: `<?xml version='1.1'`}, // Test 4.
		{Name: "declaration later is kept", In: `<project><?xml?></project>`,
			WantResult: `<project><?xml?></project>`}, // Test 5.
		{Name: "empty", In: "", WantResult: ""}, // Test 6.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := string(stripXMLDeclaration([]byte(test.In)))
			if got != test.WantResult {
				t.Errorf("stripXMLDeclaration(%q) = %q, want %q", test.In, got, test.WantResult)
			}
		})
	}
}

// TestJenkinsNameFromZipPathReadsTheAlternatingLayout pins how a job's full name is recovered from
// where its config.xml sits in an archive. Everything before the first jobs segment is whatever
// directory the archive was rooted at, and keeping it would prefix every job with the home's own
// directory name.
func TestJenkinsNameFromZipPathReadsTheAlternatingLayout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Path       string
		WantResult string
	}{
		{Name: "one level", Path: "jobs/build/config.xml", WantResult: "build"}, // Test 0.
		{Name: "nested folder", Path: "jobs/ops/jobs/build/config.xml",
			WantResult: "ops/build"}, // Test 1.
		{Name: "rooted archive", Path: "jenkins_home/jobs/build/config.xml",
			WantResult: "build"}, // Test 2.
		{Name: "windows separators", Path: `jobs\ops\jobs\build\config.xml`,
			WantResult: "ops/build"}, // Test 3.
		{Name: "no jobs segment", Path: "build/config.xml", WantResult: "build"}, // Test 4.
		{Name: "job genuinely named jobs", Path: "jobs/jobs/config.xml",
			WantResult: "jobs"}, // Test 5: the container sits at the even positions.
		{Name: "bare file", Path: "config.xml", WantResult: ""}, // Test 6.
		{Name: "empty", Path: "", WantResult: ""},               // Test 7.
		{Name: "traversal segments dropped", Path: "jobs/../../build/config.xml",
			WantResult: "build"}, // Test 8.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := jenkinsNameFromZipPath(test.Path); got != test.WantResult {
				t.Errorf("jenkinsNameFromZipPath(%q) = %q, want %q",
					test.Path, got, test.WantResult)
			}
		})
	}
}

// TestJenkinsBundleRefusesAPathWithNothingInIt pins the walker's failures. A path that holds no job
// definition is an error naming what to point at, not an empty bundle the importer would then call
// unrecognized.
func TestJenkinsBundleRefusesAPathWithNothingInIt(t *testing.T) {
	t.Parallel()
	t.Run("test 0 missing path", func(t *testing.T) {
		t.Parallel()
		if _, err := JenkinsBundle(filepath.Join(t.TempDir(), "nope")); err == nil {
			t.Error("JenkinsBundle() error = nil, want a refusal for a path that does not exist")
		}
	})
	t.Run("test 1 empty directory", func(t *testing.T) {
		t.Parallel()
		_, err := JenkinsBundle(t.TempDir())
		if err == nil {
			t.Fatal("JenkinsBundle() error = nil, want a refusal")
		}
		if !strings.Contains(err.Error(), "no config.xml found") {
			t.Errorf("error = %v, want it to name what to point at", err)
		}
	})
	t.Run("test 2 controller config is not a job", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.xml"),
			[]byte(jenkinsFreestyle("")), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		if _, err := JenkinsBundle(dir); err == nil {
			t.Error("JenkinsBundle() error = nil, want the controller's own config.xml to be " +
				"refused rather than imported as a job")
		}
	})
}

// TestJenkinsBundleEscapesAJobNameIntoItsWrapper pins that a job name holding a character that would
// end the attribute cannot be read as markup. A directory name may hold a quote, and the walker is
// the only place a name is turned into a document.
func TestJenkinsBundleEscapesAJobNameIntoItsWrapper(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	jobDir := filepath.Join(dir, "jobs", `evil"><job name="injected`)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("make fixture: %v", err)
	}
	body := jenkinsFreestyle(`<builders>
      <hudson.tasks.Shell><command>echo hi</command></hudson.tasks.Shell></builders>`)
	if err := os.WriteFile(filepath.Join(jobDir, "config.xml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	bundle, err := JenkinsBundle(dir)
	if err != nil {
		t.Fatalf("JenkinsBundle() error = %v", err)
	}
	names := JenkinsJobNames(bundle)
	if diff := cmp.Diff([]string{`evil"><job name="injected`}, names,
		cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("job names mismatch (-want +got):\n%s", diff)
	}
	plan := jenkinsPlan(t, "prod", string(bundle))
	if len(plan.Templates) != 1 {
		t.Errorf("templates = %d, want 1: the name did not forge a second job",
			len(plan.Templates))
	}
}

// TestJenkinsZipCeilingsAreEnforced pins the bounds on a hostile archive. The upload endpoint is
// reachable by anyone allowed to import, and a few kilobytes of zip can expand into gigabytes, so
// each ceiling has to refuse rather than read.
func TestJenkinsZipCeilingsAreEnforced(t *testing.T) {
	t.Parallel()
	t.Run("test 0 not a zip", func(t *testing.T) {
		t.Parallel()
		if IsJenkinsZip([]byte("<project/>")) {
			t.Error("IsJenkinsZip() = true for an XML document")
		}
		if !IsJenkinsZip([]byte("PK\x03\x04rest")) {
			t.Error("IsJenkinsZip() = false for a zip local file header")
		}
		if _, err := JenkinsBundleFromZip([]byte("PK\x03\x04not really")); err == nil {
			t.Error("JenkinsBundleFromZip() error = nil for a truncated archive")
		}
	})
	t.Run("test 1 no config.xml in it", func(t *testing.T) {
		t.Parallel()
		archive := buildZip(t, map[string]string{"jobs/build/notes.txt": "hello"})
		_, err := JenkinsBundleFromZip(archive)
		if err == nil || !strings.Contains(err.Error(), "no config.xml found") {
			t.Errorf("error = %v, want it to say no job definition was in the archive", err)
		}
	})
	t.Run("test 2 one oversized definition", func(t *testing.T) {
		t.Parallel()
		big := strings.Repeat("x", maxJenkinsConfigSize+1)
		archive := buildZip(t, map[string]string{"jobs/build/config.xml": big})
		_, err := JenkinsBundleFromZip(archive)
		if err == nil || !strings.Contains(err.Error(), "larger than a job definition") {
			t.Errorf("error = %v, want the per-entry ceiling to refuse it", err)
		}
	})
	t.Run("test 3 names are read from the tree", func(t *testing.T) {
		t.Parallel()
		archive := buildZip(t, map[string]string{
			"jenkins_home/jobs/ops/jobs/build/config.xml": jenkinsFreestyle(""),
			"jenkins_home/jobs/deploy/config.xml":         jenkinsFreestyle(""),
			"jenkins_home/jobs/deploy/builds/1/log":       "noise",
		})
		bundle, err := JenkinsBundleFromZip(archive)
		if err != nil {
			t.Fatalf("JenkinsBundleFromZip() error = %v", err)
		}
		if diff := cmp.Diff([]string{"deploy", "ops/build"}, JenkinsJobNames(bundle),
			cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("job names mismatch (-want +got):\n%s", diff)
		}
	})
}

// buildZip writes an in-memory zip archive holding the named entries, so an archive ceiling can be
// exercised without a fixture file on disk.
func buildZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, body := range entries {
		f, err := w.Create(name)
		if err != nil {
			t.Fatalf("create zip entry %q: %v", name, err)
		}
		if _, err := f.Write([]byte(body)); err != nil {
			t.Fatalf("write zip entry %q: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// TestJenkinsUsesVarMatchesOnlyTheWholeName pins the variable detector. Testing for the bare name
// reported $WORKSPACE for every script mentioning $WORKSPACE_TMP, which is a warning an operator
// learns to ignore, and missing a real use is a step that reads an empty value at run time.
func TestJenkinsUsesVarMatchesOnlyTheWholeName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Body     string
		Var      string
		WantUsed bool
	}{
		{Name: "bare", Body: "echo $WORKSPACE", Var: "WORKSPACE", WantUsed: true},     // Test 0.
		{Name: "braced", Body: "echo ${WORKSPACE}", Var: "WORKSPACE", WantUsed: true}, // Test 1.
		{Name: "followed by a slash", Body: "cd $WORKSPACE/src", Var: "WORKSPACE",
			WantUsed: true}, // Test 2.
		{Name: "longer name does not match", Body: "echo $WORKSPACE_TMP", Var: "WORKSPACE",
			WantUsed: false}, // Test 3.
		{Name: "the longer name itself", Body: "echo $WORKSPACE_TMP", Var: "WORKSPACE_TMP",
			WantUsed: true}, // Test 4.
		{Name: "several uses", Body: "echo $WORKSPACE_TMP; echo $WORKSPACE", Var: "WORKSPACE",
			WantUsed: true}, // Test 5: the scan continues past the near miss.
		{Name: "at the very end", Body: "echo $WORKSPACE", Var: "WORKSPACE", WantUsed: true}, // Test 6.
		{Name: "absent", Body: "echo hi", Var: "WORKSPACE", WantUsed: false},                 // Test 7.
		{Name: "no dollar", Body: "echo WORKSPACE", Var: "WORKSPACE", WantUsed: false},       // Test 8.
		{Name: "digit continues the name", Body: "echo $BUILD_ID2", Var: "BUILD_ID",
			WantUsed: false}, // Test 9.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := jenkinsUsesVar(test.Body, test.Var); got != test.WantUsed {
				t.Errorf("jenkinsUsesVar(%q, %q) = %v, want %v",
					test.Body, test.Var, got, test.WantUsed)
			}
		})
	}
}

// TestJenkinsDisabledJobImportsWithItsScheduleSwitchedOff pins that a job Jenkins had switched off
// does not start running after a migration. Importing its schedule enabled would begin work nobody
// expected on the first tick after the import.
func TestJenkinsDisabledJobImportsWithItsScheduleSwitchedOff(t *testing.T) {
	t.Parallel()
	doc := jenkinsFreestyle(`<disabled>true</disabled>
      <authToken>shared-secret-token</authToken>
      <assignedNode>linux-agent</assignedNode>
      <triggers><hudson.triggers.TimerTrigger><spec>0 2 * * *</spec>
      </hudson.triggers.TimerTrigger></triggers>
      <builders><hudson.tasks.Shell><command>echo hi</command></hudson.tasks.Shell></builders>`)
	plan := jenkinsPlan(t, "prod", doc)
	if len(plan.Schedules) != 1 {
		t.Fatalf("schedules = %d, want 1", len(plan.Schedules))
	}
	if plan.Schedules[0].Enabled {
		t.Error("the schedule of a disabled job imported enabled")
	}
	for _, want := range []string{"is disabled in Jenkins", "remote trigger token",
		"pinned to the Jenkins agent label"} {
		if _, ok := warningContaining(t, plan.Warnings, want); !ok {
			t.Errorf("missing %q.\nwarnings: %v", want, plan.Warnings)
		}
	}
	for _, w := range plan.Warnings {
		if strings.Contains(w, "shared-secret-token") {
			t.Errorf("the remote trigger token leaked into the report: %s", w)
		}
	}
}

// TestJenkinsStepWarningReadsAsEnglish demonstrates a defect. jenkinsStepLabel already carries the
// article for the five build-step classes it names, and jenkinsCommand's format string supplies one
// too, so an ordinary Jenkins export produces "step 1 is a a Maven step" and "is a an Ant step". The
// import report is what an operator reads to decide what is safe to run, and a line that reads as
// generated rather than checked is the one they stop reading.
func TestJenkinsStepWarningReadsAsEnglish(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Builder string
	}{
		{Name: "ant", Builder: `<hudson.tasks.Ant/>`},                // Test 0.
		{Name: "maven", Builder: `<hudson.tasks.Maven/>`},            // Test 1.
		{Name: "gradle", Builder: `<hudson.plugins.gradle.Gradle/>`}, // Test 2.
		{Name: "copy artifact",
			Builder: `<hudson.plugins.copyartifact.CopyArtifact/>`}, // Test 3.
		{Name: "junit", Builder: `<hudson.tasks.junit.JUnitResultArchiver/>`}, // Test 4.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			doc := `<jobs><job name="subject"><project><builders>` + test.Builder +
				`</builders></project></job><job name="other"><project><builders>` +
				`<hudson.tasks.Shell><command>echo hi</command></hudson.tasks.Shell>` +
				`</builders></project></job></jobs>`
			plan := jenkinsPlan(t, "prod", doc)
			for _, w := range plan.Warnings {
				if strings.Contains(w, "is a a ") || strings.Contains(w, "is a an ") {
					t.Errorf("the step warning doubles its article: %s", w)
				}
			}
		})
	}
}

// TestJenkinsAcceptsARealBareConfigXML demonstrates a defect. A real Jenkins writes its job
// configuration with an XML 1.1 declaration, and Go's encoding/xml supports only 1.0, so
// jenkinsRootElement refuses the document before anything is read. The CLI path escapes this because
// encodeJenkinsBundle strips the declaration on the way into a bundle, but the HTTP import endpoint
// documents that its body may be one config.xml and hands the raw bytes straight to FromJenkins.
// Every fixture in this package's testdata carries the same 1.1 declaration, which is what a real
// Jenkins wrote.
func TestJenkinsAcceptsARealBareConfigXML(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/jenkins-home/jobs/nightly-backup/config.xml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if !bytes.Contains(data, []byte("version='1.1'")) {
		t.Fatalf("the fixture no longer carries the declaration this test is about")
	}
	plan, err := FromJenkins("prod")(data, importNow)
	if err != nil {
		t.Fatalf("FromJenkins() on a real config.xml error = %v", err)
	}
	if len(plan.Templates) != 1 {
		t.Errorf("templates = %d, want 1", len(plan.Templates))
	}
}
