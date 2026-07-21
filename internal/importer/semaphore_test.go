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
