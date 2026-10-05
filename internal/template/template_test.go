package template_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/scrub"
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
	// A run identity token names the template a run executes, and a cloud trust policy keys on it,
	// so every launch path has to stamp it, not only the API's.
	if r.TemplateID != "tpl_1" {
		t.Errorf("TemplateID = %q, want tpl_1", r.TemplateID)
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
		// Test 7: A secret field with bounds and a sealed default is valid.
		{"secret", []template.SurveyField{
			{Var: "a", Type: template.FieldSecret, MinLength: 8, SealedDefault: "sealed"}}, false},
		// Test 8: A secret field holding a plain default would store the secret as text.
		{"secret with a plain default", []template.SurveyField{
			{Var: "a", Type: template.FieldSecret, Default: "hunter22"}}, true},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			err := template.ValidateSurvey(test.Fields)
			if (err != nil) != test.Want {
				t.Errorf("template.ValidateSurvey() error = %v, wantErr %v", err, test.Want)
			}
			// A malformed definition is the template's problem, not an answer's, and says so.
			if err != nil && !errors.Is(err, template.ErrSurveyField) {
				t.Errorf("error %v does not wrap ErrSurveyField", err)
			}
		})
	}
}

// TestASurveyFieldTypeReadsTheWordsPeopleWrite pins the spellings a survey field's type arrives in.
// A template posted with "integer" or "boolean" was refused as an unknown type, and the refusal said
// the survey answer was invalid when no answer had been given. The common spellings are read as the
// type they name, and a type that is still unknown is refused with the ones that exist.
func TestASurveyFieldTypeReadsTheWordsPeopleWrite(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   string
		Want template.FieldType
	}{
		{In: `"integer"`, Want: template.FieldInt},        // Test 0.
		{In: `"boolean"`, Want: template.FieldBool},       // Test 1.
		{In: `"string"`, Want: template.FieldText},        // Test 2.
		{In: `"textarea"`, Want: template.FieldMultiline}, // Test 3.
		{In: `"Integer"`, Want: template.FieldInt},        // Test 4: Case does not matter here.
		{In: `"int"`, Want: template.FieldInt},            // Test 5: The stored name is unchanged.
		{In: `"choice"`, Want: template.FieldChoice},      // Test 6.
		{In: `"password"`, Want: template.FieldSecret},    // Test 7: AWX's name for the secret type.
		{In: `"secret"`, Want: template.FieldSecret},      // Test 8.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var field template.SurveyField
			if err := json.Unmarshal([]byte(`{"var":"a","type":`+test.In+`}`), &field); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if field.Type != test.Want {
				t.Errorf("type %s read as %q, want %q", test.In, field.Type, test.Want)
			}
		})
	}
	err := template.ValidateSurvey([]template.SurveyField{{Var: "rate", Type: "float"}})
	if !errors.Is(err, template.ErrSurveyField) ||
		!strings.Contains(err.Error(), "text, multiline, secret, int, bool, or choice") {
		t.Errorf("an unknown type was refused with %v, want the types that exist named", err)
	}
}

// TestASecretAnswerIsNeverAPlainVar pins the split ResolveSurveyAnswers makes. A secret field is
// validated like a text field, but its answer comes back apart from the plain vars, and an
// unanswered one carries its sealed default without opening it.
func TestASecretAnswerIsNeverAPlainVar(t *testing.T) {
	t.Parallel()
	fields := []template.SurveyField{
		{Var: "env", Type: template.FieldText, Default: "stage"},
		{Var: "db_password", Type: template.FieldSecret, MinLength: 8},
		{Var: "api_token", Type: template.FieldSecret, SealedDefault: "sealed-token"},
	}
	tests := []struct {
		// Answers are the launch answers.
		Answers map[string]any
		// WantVars are the plain vars returned.
		WantVars map[string]any
		// WantSecrets are the secret answers returned.
		WantSecrets template.SecretAnswers
		// Want is the error returned.
		Want error
	}{{ // Test 0: A typed answer goes to Plain and a missing one takes its sealed default.
		Answers:  map[string]any{"db_password": "correct-horse"},
		WantVars: map[string]any{"env": "stage"},
		WantSecrets: template.SecretAnswers{
			Plain:  map[string]string{"db_password": "correct-horse"},
			Sealed: map[string]string{"api_token": "sealed-token"},
		},
	}, { // Test 1: An answer to the secret field overrides its sealed default.
		Answers:  map[string]any{"api_token": "typed-token"},
		WantVars: map[string]any{"env": "stage"},
		WantSecrets: template.SecretAnswers{
			Plain: map[string]string{"api_token": "typed-token"},
		},
	}, { // Test 2: A secret answer is held to its length bound like a text answer.
		Answers: map[string]any{"db_password": "short"}, Want: template.ErrSurvey,
	}, { // Test 3: A secret answer must be text.
		Answers: map[string]any{"db_password": 12345678}, Want: template.ErrSurvey,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			vars, secrets, err := template.ResolveSurveyAnswers(fields, test.Answers)
			if !errors.Is(err, test.Want) {
				t.Fatalf("ResolveSurveyAnswers() error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantVars, vars, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("vars mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantSecrets, secrets, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("secrets mismatch (-want +got):\n%s", diff)
			}
			plain, err := template.ResolveSurvey(fields, test.Answers)
			if !errors.Is(err, test.Want) {
				t.Fatalf("ResolveSurvey() error = %v, want %v", err, test.Want)
			}
			for _, f := range fields {
				if _, ok := plain[f.Var]; ok && f.Secret() {
					t.Errorf("ResolveSurvey() returned the secret field %q as a plain var", f.Var)
				}
			}
		})
	}
}

// TestSealDefaultsNeverKeepsASecretAsText pins how a secret default is prepared for storage and
// shown to a reader.
func TestSealDefaultsNeverKeepsASecretAsText(t *testing.T) {
	t.Parallel()
	seal := func(s string) (string, error) { return "sealed(" + s + ")", nil }
	existing := []template.SurveyField{
		{Var: "token", Type: template.FieldSecret, SealedDefault: "sealed(old)"},
	}
	tests := []struct {
		// Field is the incoming field.
		Field template.SurveyField
		// Seal is the sealing function, nil for no key.
		Seal func(string) (string, error)
		// WantSealed is the sealed default stored.
		WantSealed string
		// Want is the error returned.
		Want error
	}{{ // Test 0: A plain default is sealed.
		Field: template.SurveyField{Var: "token", Type: template.FieldSecret, Default: "new"},
		Seal:  seal, WantSealed: "sealed(new)",
	}, { // Test 1: The mask keeps the stored default.
		Field: template.SurveyField{Var: "token", Type: template.FieldSecret, Default: scrub.Marker},
		Seal:  seal, WantSealed: "sealed(old)",
	}, { // Test 2: A sealed default sent by a caller is discarded.
		Field: template.SurveyField{Var: "token", Type: template.FieldSecret, SealedDefault: "planted"},
		Seal:  seal,
	}, { // Test 3: No key, no secret default.
		Field: template.SurveyField{Var: "token", Type: template.FieldSecret, Default: "new"},
		Want:  template.ErrSurveyField,
	}, { // Test 4: The mask for a field with nothing stored stands for nothing.
		Field: template.SurveyField{Var: "other", Type: template.FieldSecret, Default: scrub.Marker},
		Seal:  seal, Want: template.ErrSurveyField,
	}, { // Test 5: A field that is not secret keeps its plain default and loses any sealed one.
		Field: template.SurveyField{
			Var: "env", Type: template.FieldText, Default: "prod", SealedDefault: "x",
		},
		Seal: seal,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := template.SealDefaults([]template.SurveyField{test.Field}, existing, test.Seal)
			if !errors.Is(err, test.Want) {
				t.Fatalf("SealDefaults() error = %v, want %v", err, test.Want)
			}
			if err != nil {
				return
			}
			if got[0].SealedDefault != test.WantSealed {
				t.Errorf("sealed default = %q, want %q", got[0].SealedDefault, test.WantSealed)
			}
			if got[0].Secret() && got[0].Default != nil {
				t.Errorf("a secret field kept a plain default: %v", got[0].Default)
			}
			if !got[0].Secret() && got[0].Default != test.Field.Default {
				t.Errorf("a plain default changed: %v", got[0].Default)
			}
			masked := template.MaskSurvey(got)
			if masked[0].SealedDefault != "" {
				t.Errorf("the masked field shows its ciphertext: %+v", masked[0])
			}
			if got[0].Secret() && test.WantSealed != "" && masked[0].Default != scrub.Marker {
				t.Errorf("the masked field reads %v, want the mask that says a default is set",
					masked[0].Default)
			}
		})
	}
}
