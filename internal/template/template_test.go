package template_test

import (
	"errors"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/templatetest"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	templatetest.Contract(t, func() template.Store { return template.NewMemStore() })
}

func TestResolveSurvey(t *testing.T) {
	t.Parallel()
	fields := []template.SurveyField{
		{Var: "env", Type: template.FieldChoice, Required: true, Choices: []string{"prod", "stage"}},
		{Var: "batch", Type: template.FieldInt, Default: 5},
		{Var: "dry", Type: template.FieldBool},
		{Var: "note", Type: template.FieldText},
	}

	got, err := template.ResolveSurvey(fields, map[string]any{
		"env": "prod", "dry": true, "note": "hi",
	})
	if err != nil {
		t.Fatalf("ResolveSurvey() error = %v", err)
	}
	if got["env"] != "prod" || got["batch"] != 5 || got["dry"] != true || got["note"] != "hi" {
		t.Errorf("resolved = %v, want prod/default 5/true/hi", got)
	}

	if _, err := template.ResolveSurvey(fields, map[string]any{}); !errors.Is(err, template.ErrSurvey) {
		t.Errorf("missing required error = %v, want ErrSurvey", err)
	}
	if _, err := template.ResolveSurvey(fields, map[string]any{"env": "banana"}); !errors.Is(err, template.ErrSurvey) {
		t.Errorf("bad choice error = %v, want ErrSurvey", err)
	}
	if _, err := template.ResolveSurvey(fields, map[string]any{"env": "prod", "batch": "five"}); !errors.Is(err, template.ErrSurvey) {
		t.Errorf("bad int error = %v, want ErrSurvey", err)
	}
}

// TestLaunchOptionsCarryThePreset pins the settings a template applies to a run however it is fired.
// The API, a schedule, and a webhook each built their own option list and had drifted: a schedule
// dropped the tool, command, dry-run flag, inventory, and image, so a Bash template fired as an
// Ansible run with no playbook, and a template saved as dry-run-only made real changes on every
// scheduled fire. They now share this one list.
func TestLaunchOptionsCarryThePreset(t *testing.T) {
	t.Parallel()
	tpl := &template.Template{
		ID: "tpl_1", Name: "nightly drift", Tool: "bash", Command: "echo drift",
		DryRun: true, InventoryID: "inv_1", ProjectID: "proj_1", Queue: "gpu",
		Timeout: 900, Image: "ghcr.io/acme/ee:9", PullCredentialID: "cred_pull",
		CredentialIDs: []string{"cred_a"},
		ExtraVars:     map[string]any{"env": "prod"},
	}
	var r run.Run
	for _, opt := range tpl.LaunchOptions() {
		opt(&r)
	}

	if !r.DryRun {
		t.Error("a dry-run template produced a run that would make real changes")
	}
	if r.Tool != "bash" {
		t.Errorf("Tool = %q, want bash; a template's tool must survive every launch path", r.Tool)
	}
	if r.Command != "echo drift" {
		t.Errorf("Command = %q, want the template's command", r.Command)
	}
	if r.InventoryID != "inv_1" {
		t.Errorf("InventoryID = %q, want inv_1", r.InventoryID)
	}
	if r.Image != "ghcr.io/acme/ee:9" {
		t.Errorf("Image = %q, want the pinned execution image", r.Image)
	}
	if r.PullCredentialID != "cred_pull" {
		t.Errorf("PullCredentialID = %q, want cred_pull", r.PullCredentialID)
	}
	if r.Timeout != 900 {
		t.Errorf("Timeout = %d, want 900", r.Timeout)
	}
	if r.Queue != "gpu" {
		t.Errorf("Queue = %q, want gpu", r.Queue)
	}
	if r.ProjectID != "proj_1" {
		t.Errorf("ProjectID = %q, want proj_1", r.ProjectID)
	}
	if len(r.CredentialIDs) != 1 || r.CredentialIDs[0] != "cred_a" {
		t.Errorf("CredentialIDs = %v, want [cred_a]", r.CredentialIDs)
	}
	if r.ExtraVars["env"] != "prod" {
		t.Errorf("ExtraVars = %v, want env=prod", r.ExtraVars)
	}
}
