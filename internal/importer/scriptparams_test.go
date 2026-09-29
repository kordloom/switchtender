package importer_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/importer"
)

// runImported runs an imported script the way a Bash run does, with the answers given as the
// SWITCHTENDER_VAR_ entries the runner sets, and returns what it printed.
func runImported(t *testing.T, command string, answers map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	cmd := exec.Command("bash", "-c", command)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	for name, value := range answers {
		cmd.Env = append(cmd.Env, "SWITCHTENDER_VAR_"+name+"="+value)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the imported script failed: %v\n%s\nscript:\n%s", err, out, command)
	}
	return strings.TrimSpace(string(out))
}

// TestImportedJenkinsJobReadsItsParameters pins that a Jenkins job's script receives its parameters
// under the names it reads. Jenkins sets each one as an environment variable, and they arrived here
// only inside the JSON of all the answers, so the script ran with every parameter empty.
func TestImportedJenkinsJobReadsItsParameters(t *testing.T) {
	t.Parallel()
	doc := freestyle(`<properties><hudson.model.ParametersDefinitionProperty><parameterDefinitions>
	<hudson.model.StringParameterDefinition><name>TARGET</name>
		<defaultValue>staging</defaultValue></hudson.model.StringParameterDefinition>
	<hudson.model.BooleanParameterDefinition><name>DRY_RUN</name>
		<defaultValue>true</defaultValue></hudson.model.BooleanParameterDefinition>
	<hudson.model.StringParameterDefinition><name>NOTE</name></hudson.model.StringParameterDefinition>
	</parameterDefinitions></hudson.model.ParametersDefinitionProperty></properties>` +
		shellStep(`echo "target=$TARGET dry=$DRY_RUN note=[$NOTE]"`))
	plan, err := importer.FromJenkins("prod")(jenkinsBundle([2]string{"deploy", doc}), fixedTime)
	if err != nil {
		t.Fatalf("FromJenkins() error = %v", err)
	}
	command := plan.Templates[0].Command
	tests := []struct {
		Answers    map[string]string
		WantResult string
	}{{ // Test 0: Nothing answered, as on a scheduled fire: the job's defaults.
		WantResult: "target=staging dry=true note=[]",
	}, { // Test 1: Answers win, including one with a space and a quote in it.
		Answers:    map[string]string{"TARGET": "prod east", "DRY_RUN": "false", "NOTE": `it's "done"`},
		WantResult: `target=prod east dry=false note=[it's "done"]`,
	}}
	for _, test := range tests {
		if diff := cmp.Diff(test.WantResult, runImported(t, command, test.Answers)); diff != "" {
			t.Errorf("script output mismatch (-want +got):\n%s", diff)
		}
	}
}

// TestImportedRundeckJobReadsItsOptions pins the same for Rundeck, which sets RD_OPTION_ variables
// and also replaces @option.name@ and ${option.name} in a step's text before running it. Left as
// written, the first ran as a literal word and the second stopped Bash with a bad substitution.
func TestImportedRundeckJobReadsItsOptions(t *testing.T) {
	t.Parallel()
	const doc = `- name: deploy
  options:
    - name: version
      value: "1.0"
    - name: region-name
      value: us-east-1
  sequence:
    commands:
      - exec: echo "env=$RD_OPTION_VERSION token=@option.version@ brace=${option.version} region=$RD_OPTION_REGION_NAME"
`
	plan, err := importer.FromRundeck("prod")([]byte(doc), fixedTime)
	if err != nil {
		t.Fatalf("FromRundeck() error = %v", err)
	}
	command := plan.Templates[0].Command
	if got, want := runImported(t, command, nil), "env=1.0 token=1.0 brace=1.0 region=us-east-1"; got != want {
		t.Errorf("with no answers the script printed %q, want the options' defaults %q", got, want)
	}
	got := runImported(t, command, map[string]string{"version": "2.5"})
	if want := "env=2.5 token=2.5 brace=2.5 region=us-east-1"; got != want {
		t.Errorf("with an answer the script printed %q, want %q", got, want)
	}
}
