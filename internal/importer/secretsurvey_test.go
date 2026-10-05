package importer

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// TestASecretPromptIsNoLongerReportedAsLeftOut pins the assessment for the four secret prompts the
// importers used to refuse: an AWX password field, a Semaphore secret variable, a Rundeck secure
// option, and a Jenkins password parameter. Each now arrives as a secret survey field, so the report
// a prospect reads before migrating must stop listing it under what does not come across. A default
// the export held only as a placeholder or ciphertext is something to set again, the way a
// credential shell's secret is, so it is listed for review and its value never appears.
func TestASecretPromptIsNoLongerReportedAsLeftOut(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which source is read.
		Name string
		// Plan reads the source's export.
		Plan func(t *testing.T) *Plan
		// Var is the secret prompt's variable.
		Var string
		// WantReview is whether its missing default must be listed for review.
		WantReview bool
	}{{ // Test 0: An AWX password field, whose default AWX exports as $encrypted$.
		Name: "awx", Var: "vault_pass", WantReview: true,
		Plan: func(t *testing.T) *Plan {
			t.Helper()
			plan, err := FromAWX([]byte(`{"job_templates": [{"name": "j", "playbook": "p.yml",
				"survey_spec": {"spec": [{"variable": "vault_pass", "type": "password",
				"default": "$encrypted$"}]}}]}`), importNow)
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			return plan
		},
	}, { // Test 1: A Semaphore secret variable, which carries no default.
		Name: "semaphore", Var: "api_token",
		Plan: func(t *testing.T) *Plan {
			t.Helper()
			plan, err := FromSemaphore([]byte(`{"meta": {"name": "ops"}, "templates": [{"name": "s",
				"playbook": "s.yml", "survey_vars": [{"name": "api_token", "type": "secret"}]}]}`),
				importNow)
			if err != nil {
				t.Fatalf("FromSemaphore() error = %v", err)
			}
			return plan
		},
	}, { // Test 2: A Rundeck secure option.
		Name: "rundeck", Var: "db_password",
		Plan: func(t *testing.T) *Plan {
			t.Helper()
			return rundeckPlan(t, "prod", "- name: j\n  options:\n    - name: db_password\n"+
				"      secure: true\n  sequence:\n    commands:\n      - exec: /bin/x\n")
		},
	}, { // Test 3: A Jenkins password parameter with its encrypted default.
		Name: "jenkins", Var: "TOKEN", WantReview: true,
		Plan: func(t *testing.T) *Plan {
			t.Helper()
			return jenkinsPlan(t, "prod", jenkinsFreestyle(`<properties>
				<hudson.model.ParametersDefinitionProperty><parameterDefinitions>
				<hudson.model.PasswordParameterDefinition><name>TOKEN</name>
				<defaultValue>{AQAAABAAAAAQcipher}</defaultValue>
				</hudson.model.PasswordParameterDefinition>
				</parameterDefinitions></hudson.model.ParametersDefinitionProperty></properties>
				<builders><hudson.tasks.Shell><command>echo hi</command></hudson.tasks.Shell></builders>`))
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			plan := test.Plan(t)
			a := plan.Assess()
			for _, w := range a.Report.LeftOut {
				if strings.Contains(w, test.Var) {
					t.Errorf("the secret prompt is still listed as not coming across: %s", w)
				}
			}
			var reviewed bool
			for _, w := range a.Report.NeedsReview {
				if strings.Contains(w, test.Var) {
					reviewed = true
				}
			}
			if reviewed != test.WantReview {
				t.Errorf("listed for review = %v, want %v: %v", reviewed, test.WantReview,
					a.Report.NeedsReview)
			}
			var out bytes.Buffer
			Render(&out, test.Name, "export", a)
			for _, leaked := range []string{"$encrypted$", "AQAAABAAAAAQcipher"} {
				if strings.Contains(out.String(), leaked) {
					t.Errorf("the assessment repeats the exported default %q:\n%s", leaked, out.String())
				}
			}
			var secret bool
			for _, f := range plan.Templates[0].Survey {
				if f.Var == test.Var && f.Secret() && f.Default == nil && f.SealedDefault == "" {
					secret = true
				}
			}
			if !secret {
				t.Errorf("survey = %+v, want %s as a secret field with no default",
					plan.Templates[0].Survey, test.Var)
			}
		})
	}
}
