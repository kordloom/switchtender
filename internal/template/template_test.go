package template_test

import (
	"errors"
	"testing"

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
