package template_test

import (
	"errors"
	"fmt"
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

// TestResolveSurveyConstraints proves the launch-time survey checks: an int range, a text length and
// pattern, a multiline field, and the errors when an answer breaks a rule.
func TestResolveSurveyConstraints(t *testing.T) {
	t.Parallel()
	min, max := 1, 10
	fields := []template.SurveyField{
		{Var: "count", Type: template.FieldInt, Min: &min, Max: &max},
		{Var: "name", Type: template.FieldText, MinLength: 3, MaxLength: 8, Pattern: "^[a-z]+$"},
		{Var: "notes", Type: template.FieldMultiline},
	}
	tests := []struct {
		Name    string
		Answers map[string]any
		WantErr bool
	}{
		{"all valid", map[string]any{"count": float64(5), "name": "web", "notes": "line1\nline2"}, false},
		{"int too high", map[string]any{"count": float64(99), "name": "web"}, true},
		{"int too low", map[string]any{"count": float64(0), "name": "web"}, true},
		{"text too short", map[string]any{"count": float64(5), "name": "ab"}, true},
		{"text too long", map[string]any{"count": float64(5), "name": "abcdefghij"}, true},
		{"text bad pattern", map[string]any{"count": float64(5), "name": "Web1"}, true},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			_, err := template.ResolveSurvey(fields, test.Answers)
			if (err != nil) != test.WantErr {
				t.Errorf("template.ResolveSurvey() error = %v, wantErr %v", err, test.WantErr)
			}
		})
	}
}

// TestResolveSurveyPatternAnchored proves a survey pattern constrains the whole answer rather than
// matching a substring of it.
//
// A bare regexp matches anywhere, so a field whose pattern is a four digit code accepted anything
// that merely contained four digits. An author writing a format constraint means the format of the
// answer, and the value is injected into a play as an extra var, so a pattern that lets arbitrary
// text ride alongside the match is not a constraint at all.
func TestResolveSurveyPatternAnchored(t *testing.T) {
	t.Parallel()
	fields := []template.SurveyField{
		{Var: "code", Type: template.FieldText, Pattern: `\d{4}`},
		{Var: "host", Type: template.FieldText, Pattern: `[a-z0-9-]+`},
	}
	tests := []struct {
		Name    string
		Answers map[string]any
		WantErr bool
	}{ // Test 0: An answer that is exactly the pattern is accepted.
		{"exact match", map[string]any{"code": "1234", "host": "web-01"}, false},
		// Test 1: Extra text around the match must be refused, not accepted on the substring.
		{"substring only", map[string]any{"code": "abc1234xyz", "host": "web-01"}, true},
		// Test 2: A trailing shell fragment is the case that makes this matter.
		{"trailing payload", map[string]any{"code": "1234; rm -rf /", "host": "web-01"}, true},
		// Test 3: A leading fragment is refused the same way.
		{"leading payload", map[string]any{"code": "1234", "host": "BAD web-01"}, true},
		// Test 4: A newline cannot be used to smuggle a second line past an anchored pattern.
		{"newline smuggle", map[string]any{"code": "1234", "host": "web-01\nrogue"}, true},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			_, err := template.ResolveSurvey(fields, test.Answers)
			if (err != nil) != test.WantErr {
				t.Errorf("template.ResolveSurvey() error = %v, wantErr %v", err, test.WantErr)
			}
		})
	}
}

// TestResolveSurveyRequiredRejectsEmpty proves a required text field is not satisfied by an empty
// string. Only a missing key was refused, so sending the field with no value passed a check the
// operator set precisely to make the value mandatory.
func TestResolveSurveyRequiredRejectsEmpty(t *testing.T) {
	t.Parallel()
	fields := []template.SurveyField{{Var: "env", Type: template.FieldText, Required: true}}

	if _, err := template.ResolveSurvey(fields, map[string]any{"env": ""}); !errors.Is(err, template.ErrSurvey) {
		t.Errorf("empty answer to a required field = %v, want ErrSurvey", err)
	}
	if _, err := template.ResolveSurvey(fields, map[string]any{}); !errors.Is(err, template.ErrSurvey) {
		t.Errorf("missing required field = %v, want ErrSurvey", err)
	}
	got, err := template.ResolveSurvey(fields, map[string]any{"env": "prod"})
	if err != nil {
		t.Fatalf("template.ResolveSurvey() error = %v", err)
	}
	if got["env"] != "prod" {
		t.Errorf("env = %v, want prod", got["env"])
	}
}

// TestResolveSurveyLengthCountsRunes proves length bounds count characters rather than bytes, so a
// multibyte answer is measured the way the person typing it reads it.
func TestResolveSurveyLengthCountsRunes(t *testing.T) {
	t.Parallel()
	fields := []template.SurveyField{
		{Var: "label", Type: template.FieldText, MinLength: 2, MaxLength: 8},
	}
	// Five characters, fifteen bytes. Byte counting rejected this against a limit of eight.
	if _, err := template.ResolveSurvey(fields, map[string]any{"label": "東京都市圏"}); err != nil {
		t.Errorf("five character multibyte answer = %v, want it accepted under a max of 8", err)
	}
	// Nine characters must still be refused, so the bound is not simply gone.
	if _, err := template.ResolveSurvey(fields, map[string]any{"label": "abcdefghi"}); err == nil {
		t.Error("nine character answer was accepted, want it refused against a max of 8")
	}
	// One multibyte character is below a minimum of two and must be refused.
	if _, err := template.ResolveSurvey(fields, map[string]any{"label": "東"}); err == nil {
		t.Error("one character answer was accepted, want it refused against a min of 2")
	}
}

// TestValidateSurvey proves a malformed survey definition is caught when the template is written
// rather than on every launch afterward.
func TestValidateSurvey(t *testing.T) {
	t.Parallel()
	low, high := 5, 1
	tests := []struct {
		Name   string
		Fields []template.SurveyField
		Want   bool
	}{ // Test 0: A well formed survey passes.
		{"valid", []template.SurveyField{
			{Var: "a", Type: template.FieldText, Pattern: `^[a-z]+$`},
			{Var: "b", Type: template.FieldChoice, Choices: []string{"x"}},
			{Var: "c", Type: template.FieldInt},
		}, false},
		// Test 1: An uncompilable pattern is refused at save, not at launch.
		{"bad pattern", []template.SurveyField{
			{Var: "a", Type: template.FieldText, Pattern: "[unclosed"}}, true},
		// Test 2: A choice field offering nothing can never be answered.
		{"choice with no choices", []template.SurveyField{
			{Var: "a", Type: template.FieldChoice}}, true},
		// Test 3: Inverted integer bounds admit no answer at all.
		{"min above max", []template.SurveyField{
			{Var: "a", Type: template.FieldInt, Min: &low, Max: &high}}, true},
		// Test 4: Inverted length bounds admit no answer at all.
		{"min length above max length", []template.SurveyField{
			{Var: "a", Type: template.FieldText, MinLength: 9, MaxLength: 2}}, true},
		// Test 5: An unknown type is refused here as it is at launch.
		{"unknown type", []template.SurveyField{
			{Var: "a", Type: template.FieldType("wat")}}, true},
		// Test 6: An empty survey is valid.
		{"empty", nil, false},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			err := template.ValidateSurvey(test.Fields)
			if (err != nil) != test.Want {
				t.Errorf("template.ValidateSurvey() error = %v, wantErr %v", err, test.Want)
			}
			if err != nil && !errors.Is(err, template.ErrSurvey) {
				t.Errorf("error %v does not wrap ErrSurvey", err)
			}
		})
	}
}
