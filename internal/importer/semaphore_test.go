package importer_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

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

// TestSemaphoreSecretSurveyImportsAsSecret covers the mirror of a rule the AWX importer holds.
// Semaphore's survey variables have a "secret" type, prompted for and stored obscured on their
// side. It imports as a secret field, whose answer is sealed with the credential key and never kept
// on the run in plain text, so the migration is complete without being less safe than what the
// operator left.
func TestSemaphoreSecretSurveyImportsAsSecret(t *testing.T) {
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
	want := []template.SurveyField{
		{Var: "env", Label: "Environment", Type: template.FieldText},
		{Var: "vault_pass", Label: "Vault password", Type: template.FieldSecret, Required: true},
	}
	if diff := cmp.Diff(want, plan.Templates[0].Survey, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("survey mismatch (-want +got):\n%s", diff)
	}
	for _, w := range plan.Warnings {
		if strings.Contains(w, "vault_pass") {
			t.Errorf("the secret variable is still reported as left out: %s", w)
		}
	}
}

// TestSemaphoreImportsAProjectBackup pins the shape Semaphore's own backup writes: a flat
// single-project document whose top level is the project itself.
//
// The importer read only a `projects` wrapper, which Semaphore does not produce. A real backup
// therefore matched nothing and was refused with "nothing in this document was recognized", so the
// documented one-command migration failed on the only file a Semaphore operator can actually
// produce. The fixture mirrors BackupFormat's top-level keys.
func TestSemaphoreImportsAProjectBackup(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "semaphore-backup.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	plan, err := importer.FromSemaphore(raw, fixedTime)
	if err != nil {
		t.Fatalf("FromSemaphore() error = %v", err)
	}

	if len(plan.Projects) != 1 {
		t.Fatalf("projects = %d, want the backup's one repository", len(plan.Projects))
	}
	if got := plan.Projects[0].RepoURL; got != "https://git.example.com/infra.git" {
		t.Errorf("repo url = %q, want the backup's git_url", got)
	}
	if len(plan.Inventories) != 1 {
		t.Fatalf("inventories = %d, want 1", len(plan.Inventories))
	}
	if !strings.Contains(plan.Inventories[0].Content, "web01.acme.internal") {
		t.Errorf("inventory lost its hosts:\n%s", plan.Inventories[0].Content)
	}
	if len(plan.Credentials) != 1 {
		t.Errorf("credentials = %d, want the backup's one key", len(plan.Credentials))
	}
	if len(plan.Templates) != 1 {
		t.Fatalf("templates = %d, want 1", len(plan.Templates))
	}
	if got := plan.Templates[0].Playbook; got != "site.yml" {
		t.Errorf("playbook = %q, want site.yml", got)
	}
	if len(plan.Templates[0].Survey) != 1 {
		t.Errorf("survey vars = %d, want the one the backup carries", len(plan.Templates[0].Survey))
	}
	if len(plan.Schedules) != 1 {
		t.Fatalf("schedules = %d, want 1", len(plan.Schedules))
	}
	if got := plan.Schedules[0].Cron; got != "0 2 * * *" {
		t.Errorf("cron = %q, want 0 2 * * *", got)
	}
}

// TestSemaphoreImportsARealCurrentBackup reads a backup taken verbatim from a live current
// Semaphore release (v2.16 era, sqlite dialect) whose top level carries fields older fixtures
// never saw: workflows, integrations, integration_aliases, runners, roles, views, and
// secret_storages. The importer must read through the unknown fields and land every object.
// The two prior real-world failures were both fixture-shaped gaps; this fixture is the real shape.
func TestSemaphoreImportsARealCurrentBackup(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "semaphore_real_v2_backup.json"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	plan, err := importer.FromSemaphore(raw, fixedTime)
	if err != nil {
		t.Fatalf("FromSemaphore() error = %v", err)
	}
	if len(plan.Projects) != 1 || plan.Projects[0].RepoURL != "https://example.test/infra.git" {
		t.Fatalf("projects = %+v, want the backup's one repository", plan.Projects)
	}
	if len(plan.Inventories) != 1 || !strings.Contains(plan.Inventories[0].Content, "web01 ansible_host=10.0.0.11") {
		t.Fatalf("inventories lost content: %+v", plan.Inventories)
	}
	if len(plan.Templates) != 3 {
		t.Fatalf("templates = %d, want the backup's three", len(plan.Templates))
	}
	if len(plan.Schedules) != 1 || plan.Schedules[0].Cron != "0 3 * * *" {
		t.Fatalf("schedules = %+v, want the 3am cron", plan.Schedules)
	}
	// Both keys in this backup are type none, which Semaphore writes as stored and which holds no
	// secret. They used to import as shells asking for secrets that never existed.
	if len(plan.Credentials) != 0 {
		t.Errorf("credentials = %d, want none from two keys that hold nothing", len(plan.Credentials))
	}
	if !slices.ContainsFunc(plan.Warnings, func(w string) bool {
		return strings.Contains(w, `"None", "deploy-key"`) && strings.Contains(w, "type none")
	}) {
		t.Errorf("the keys that hold nothing were not named: %v", plan.Warnings)
	}
}
