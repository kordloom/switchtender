package importer_test

import (
	"os"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/importer"
	"github.com/kordloom/switchtender/internal/template"
)

func TestFromSemaphore(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/semaphore-export.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	plan, err := importer.FromSemaphore(data, fixedTime)
	if err != nil {
		t.Fatalf("FromSemaphore() error = %v", err)
	}

	if len(plan.Projects) != 1 || plan.Projects[0].RepoURL != "git@github.com:acme/infra.git" {
		t.Fatalf("projects = %+v, want one from the main repo", plan.Projects)
	}
	if len(plan.Inventories) != 1 || plan.Inventories[0].Content != "[all]\nstage1 ansible_connection=local\n" {
		t.Fatalf("inventories = %+v, want the static content preserved", plan.Inventories)
	}
	if len(plan.Credentials) != 2 {
		t.Fatalf("credentials = %d, want 2 key shells", len(plan.Credentials))
	}

	if len(plan.Templates) != 1 {
		t.Fatalf("templates = %d, want 1", len(plan.Templates))
	}
	tpl := plan.Templates[0]
	if tpl.ProjectID != plan.Projects[0].ID || tpl.InventoryID != plan.Inventories[0].ID {
		t.Errorf("template not wired to project/inventory: %+v", tpl)
	}
	wantSurvey := []template.SurveyField{
		{Var: "env", Label: "Environment", Type: template.FieldChoice, Required: true, Choices: []string{"stage", "prod"}},
		{Var: "batch", Label: "Batch size", Type: template.FieldInt},
	}
	if diff := cmp.Diff(wantSurvey, tpl.Survey); diff != "" {
		t.Errorf("survey mismatch (-want +got):\n%s", diff)
	}

	if len(plan.Schedules) != 1 || plan.Schedules[0].Cron != "0 * * * *" || plan.Schedules[0].TemplateID != tpl.ID {
		t.Fatalf("schedules = %+v, want the hourly cron wired to the template", plan.Schedules)
	}

	assertWarns(t, plan.Warnings, "needs its secret re-entered")
}

// TestSemaphoreSecretSurveyIsNotImportedAsPlainText covers the mirror of a rule the AWX importer
// already holds. Semaphore's survey variables have a "secret" type, prompted for and stored obscured
// on their side. A survey field here is plain text whose answer is kept on the run and injected as an
// extra var, so importing one turned a secret prompt into a value stored in the clear on every run of
// that template, in its record, its exports, and the evidence drawn from it. A migration that looks
// complete and is less safe than what the operator left is worse than one that says what it skipped.
func TestSemaphoreSecretSurveyIsNotImportedAsPlainText(t *testing.T) {
	t.Parallel()
	export := []byte(`{
	  "projects": [{
	    "name": "acme",
	    "templates": [{
	      "name": "deploy",
	      "playbook": "site.yml",
	      "survey_vars": [
	        {"name": "env", "title": "Environment", "type": "string"},
	        {"name": "vault_pass", "title": "Vault password", "type": "secret", "required": true}
	      ]
	    }]
	  }]
	}`)
	plan, err := importer.FromSemaphore(export, fixedTime)
	if err != nil {
		t.Fatalf("FromSemaphore() error = %v", err)
	}
	if len(plan.Templates) != 1 {
		t.Fatalf("templates = %d, want 1", len(plan.Templates))
	}
	for _, f := range plan.Templates[0].Survey {
		if f.Var == "vault_pass" {
			t.Errorf("the secret survey variable was imported as a %q field, so its answer would be "+
				"typed in the clear and stored on every run", f.Type)
		}
	}
	if len(plan.Templates[0].Survey) != 1 || plan.Templates[0].Survey[0].Var != "env" {
		t.Errorf("survey = %+v, want the ordinary field kept", plan.Templates[0].Survey)
	}
	assertWarns(t, plan.Warnings, "vault_pass")
	assertWarns(t, plan.Warnings, "credential")
}
