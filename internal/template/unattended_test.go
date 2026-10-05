package template_test

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// TestResolveSurveyDefaultsAnswersEveryQuestionItCan pins how a launch with nobody present to answer
// a survey fills it in: a schedule's fire, a webhook, a pull request plan, or a callback.
//
// Before this a schedule and a webhook fired with the template's extra vars alone and never looked
// at the survey, so an optional question's default never reached the play, a secret question's
// sealed default never reached it either, and a required question nobody answered fired anyway.
func TestResolveSurveyDefaultsAnswersEveryQuestionItCan(t *testing.T) {
	t.Parallel()
	five, one := 5, 1
	tests := []struct {
		// Fields is the survey.
		Fields []template.SurveyField
		// WantVars are the plain values the launch carries.
		WantVars map[string]any
		// WantSealed are the sealed defaults that ride onto the run unopened.
		WantSealed map[string]string
		// WantUnanswered are the questions the refusal names, in survey order.
		WantUnanswered []string
		// WantReason is a fragment the refusal must contain.
		WantReason string
		// Want is the error, nil for a launch that goes ahead.
		Want error
	}{{ // Test 0: An optional question takes its default, as a launch that leaves it blank does.
		Fields:   []template.SurveyField{{Var: "region", Type: template.FieldText, Default: "eu"}},
		WantVars: map[string]any{"region": "eu"},
	}, { // Test 1: An optional secret question takes its sealed default, unopened.
		Fields: []template.SurveyField{
			{Var: "token", Type: template.FieldSecret, SealedDefault: "sealed-token"},
		},
		WantSealed: map[string]string{"token": "sealed-token"},
	}, { // Test 2: A required question takes its default, held to the rules an answer is held to.
		Fields: []template.SurveyField{
			{Var: "batch", Type: template.FieldInt, Required: true, Default: float64(5), Min: &one},
		},
		WantVars: map[string]any{"batch": 5},
	}, { // Test 3: A required secret question takes its sealed default.
		Fields: []template.SurveyField{
			{Var: "db_password", Type: template.FieldSecret, Required: true, SealedDefault: "sealed-pw"},
		},
		WantSealed: map[string]string{"db_password": "sealed-pw"},
	}, { // Test 4: A required question with no default has no answer, so the launch is refused.
		Fields:         []template.SurveyField{{Var: "release", Type: template.FieldText, Required: true}},
		WantUnanswered: []string{"release"}, WantReason: `"release" is required and has no default`,
		Want: template.ErrUnanswered,
	}, { // Test 5: A required secret question with no sealed default is refused the same way.
		Fields: []template.SurveyField{
			{Var: "db_password", Type: template.FieldSecret, Required: true},
		},
		WantUnanswered: []string{"db_password"}, WantReason: `"db_password" is required`,
		Want: template.ErrUnanswered,
	}, { // Test 6: An empty default is not an answer to a required question.
		Fields: []template.SurveyField{
			{Var: "release", Type: template.FieldText, Required: true, Default: ""},
		},
		WantUnanswered: []string{"release"}, Want: template.ErrUnanswered,
	}, { // Test 7: A default the question's own rules refuse cannot stand in for an answer.
		Fields: []template.SurveyField{
			{Var: "env", Type: template.FieldChoice, Required: true, Default: "qa",
				Choices: []string{"prod", "stage"}},
		},
		WantUnanswered: []string{"env"}, WantReason: "its default is not a valid answer",
		Want: template.ErrUnanswered,
	}, { // Test 8: Every unanswered question is named, in survey order, not only the first.
		Fields: []template.SurveyField{
			{Var: "release", Type: template.FieldText, Required: true},
			{Var: "region", Type: template.FieldText, Default: "eu"},
			{Var: "db_password", Type: template.FieldSecret, Required: true},
		},
		WantUnanswered: []string{"release", "db_password"}, Want: template.ErrUnanswered,
	}, { // Test 9: An optional question with no default adds nothing.
		Fields: []template.SurveyField{
			{Var: "note", Type: template.FieldText},
			{Var: "token", Type: template.FieldSecret},
		},
	}, { // Test 10: A required toggle whose default is false is answered false, not refused.
		Fields:   []template.SurveyField{{Var: "force", Type: template.FieldBool, Required: true, Default: false}},
		WantVars: map[string]any{"force": false},
	}, { // Test 11: An optional default is applied as an interactive launch applies it, unvalidated,
		// so the two launches never carry different values for the same template.
		Fields:   []template.SurveyField{{Var: "batch", Type: template.FieldInt, Default: "", Max: &five}},
		WantVars: map[string]any{"batch": ""},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			vars, secrets, err := template.ResolveSurveyDefaults(test.Fields)
			if !errors.Is(err, test.Want) {
				t.Fatalf("ResolveSurveyDefaults() error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantUnanswered, template.UnansweredVars(err),
				cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("unanswered questions (-want +got):\n%s", diff)
			}
			if test.WantReason != "" && !strings.Contains(fmt.Sprint(err), test.WantReason) {
				t.Errorf("refusal %q does not contain %q", err, test.WantReason)
			}
			if err != nil {
				return
			}
			if diff := cmp.Diff(test.WantVars, vars, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("plain values (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantSealed, secrets.Sealed, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("sealed defaults (-want +got):\n%s", diff)
			}
			if len(secrets.Plain) > 0 {
				t.Errorf("a launch nobody answered produced plain secret answers: %v", secrets.Plain)
			}
		})
	}
}

// TestUnattendedVarsLayersTheSurveyOverTheTemplate pins what a schedule or a webhook carries: the
// template's own extra vars, the survey's defaults over them, and a secret question's sealed default
// as the only value its variable has.
func TestUnattendedVarsLayersTheSurveyOverTheTemplate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Template is the template fired.
		Template *template.Template
		// WantVars are the plain variables.
		WantVars map[string]any
		// WantSealed are the sealed defaults.
		WantSealed map[string]string
	}{{ // Test 0: No survey carries the template's own vars and nothing sealed.
		Template: &template.Template{ExtraVars: map[string]any{"env": "prod"}},
		WantVars: map[string]any{"env": "prod"},
	}, { // Test 1: A survey default wins over a template extra var of the same name, as an answer does.
		Template: &template.Template{
			ExtraVars: map[string]any{"env": "prod", "region": "us"},
			Survey:    []template.SurveyField{{Var: "region", Type: template.FieldText, Default: "eu"}},
		},
		WantVars: map[string]any{"env": "prod", "region": "eu"},
	}, { // Test 2: A template extra var named like a secret question is dropped for its sealed default.
		Template: &template.Template{
			ExtraVars: map[string]any{"db_password": "plain-template-value", "env": "prod"},
			Survey: []template.SurveyField{
				{Var: "db_password", Type: template.FieldSecret, SealedDefault: "sealed-default"},
			},
		},
		WantVars:   map[string]any{"env": "prod"},
		WantSealed: map[string]string{"db_password": "sealed-default"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			before := maps.Clone(test.Template.ExtraVars)
			vars, sealed, err := test.Template.UnattendedVars()
			if err != nil {
				t.Fatalf("UnattendedVars() error = %v", err)
			}
			if diff := cmp.Diff(test.WantVars, vars, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("plain variables (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantSealed, sealed, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("sealed defaults (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(before, test.Template.ExtraVars); diff != "" {
				t.Errorf("resolving the survey changed the template's own extra vars (-before "+
					"+after):\n%s", diff)
			}
		})
	}
}

// TestUnattendedOptionsLeaveNoPlainValueBesideASealedOne pins the case run.WithExtraVars cannot
// express. A template whose only extra var shares its name with a secret question resolves to no
// plain variables at all, and WithExtraVars leaves the template's own value in place when handed an
// empty map, so the plain value sat on the run, in every read and receipt, beside the sealed answer
// it was meant to give way to.
func TestUnattendedOptionsLeaveNoPlainValueBesideASealedOne(t *testing.T) {
	t.Parallel()
	tpl := &template.Template{
		ID: "tpl_1", Playbook: "site.yml",
		ExtraVars: map[string]any{"db_password": "plain-template-value"},
		Survey: []template.SurveyField{
			{Var: "db_password", Type: template.FieldSecret, Required: true, SealedDefault: "sealed-default"},
		},
	}
	survey, err := tpl.UnattendedOptions()
	if err != nil {
		t.Fatalf("UnattendedOptions() error = %v", err)
	}
	r := &run.Run{ID: "run_1"}
	run.ApplyOptions(r, append(tpl.LaunchOptions(), survey...))
	if _, ok := r.ExtraVars["db_password"]; ok {
		t.Errorf("the run carries the template's plain value beside the sealed default: %v", r.ExtraVars)
	}
	if diff := cmp.Diff(map[string]string{"db_password": "sealed-default"}, r.SealedVars); diff != "" {
		t.Errorf("sealed vars (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(run.SealedDigestsOf(r.SealedVars), r.SealedDigests); diff != "" {
		t.Errorf("the run does not bind its sealed default by digest (-want +got):\n%s", diff)
	}
}

// TestRefuseUnattendedSaysWhatToDo pins the refusal a schedule, a webhook, and a pull request plan
// record: it names each question, the launch, and the template, says the two ways out, and still
// matches ErrUnanswered for a caller deciding what to do with it.
func TestRefuseUnattendedSaysWhatToDo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Fields is the survey.
		Fields []template.SurveyField
		// WantParts must each appear in the refusal.
		WantParts []string
	}{{ // Test 0: One question.
		Fields: []template.SurveyField{{Var: "db_password", Type: template.FieldSecret, Required: true}},
		WantParts: []string{`"db_password" is required and has no default`, "scheduled fire",
			`template "rotate"`, "give the question a default", "launch the template by hand"},
	}, { // Test 1: Two questions are both named, and the fix speaks to each.
		Fields: []template.SurveyField{
			{Var: "release", Type: template.FieldText, Required: true},
			{Var: "db_password", Type: template.FieldSecret, Required: true},
		},
		WantParts: []string{`"release"`, `"db_password"`, "give each question a default"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			tpl := &template.Template{Name: "rotate", Survey: test.Fields}
			_, cause := tpl.UnattendedOptions()
			if cause == nil {
				t.Fatal("UnattendedOptions() succeeded for a survey nobody can answer")
			}
			got := tpl.RefuseUnattended("scheduled fire", cause)
			if !errors.Is(got, template.ErrUnanswered) {
				t.Errorf("the refusal %v does not match ErrUnanswered", got)
			}
			for _, part := range test.WantParts {
				if !strings.Contains(got.Error(), part) {
					t.Errorf("the refusal %q does not contain %q", got, part)
				}
			}
		})
	}
}
