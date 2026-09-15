package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// TestAnswersToASurveylessTemplateAreRefused covers input the product accepted and threw away.
//
// The survey branch only runs for a template carrying one, so answers sent to a template without a
// survey were a silent no-op: 201, the run started, and the values the caller believed they were
// passing were nowhere. Both answers and extra_vars end up in the same place on the run, so
// mistaking one for the other is the natural error, and it produced no signal at all.
//
// It is the same rule the branch beside it already enforces in the other direction: a launch that
// writes around the survey is refused rather than quietly preferred.
func TestAnswersToASurveylessTemplateAreRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which launch is being made.
		Name string
		// Survey is the template's survey, empty for a template that asks nothing.
		Survey []template.SurveyField
		// Body is the launch request.
		Body string
		// WantRefused is whether the launch must be refused.
		WantRefused bool
		// WantVar is a variable the submitted run must carry, when it is submitted.
		WantVar string
	}{{ // Test 0: Answers to a template that asks nothing. Refused rather than dropped.
		Name: "answers with no survey", Survey: nil,
		Body: `{"answers":{"environment":"staging"}}`, WantRefused: true,
	}, { // Test 1: The same values under extra_vars, which is where they belong, are accepted.
		Name: "extra vars with no survey", Survey: nil,
		Body: `{"extra_vars":{"environment":"staging"}}`, WantVar: "environment",
	}, { // Test 2: Answers to a template that does ask are accepted and reach the run.
		Name: "answers with a survey",
		Survey: []template.SurveyField{{
			Var: "environment", Label: "Environment", Type: template.FieldChoice,
			Required: true, Choices: []string{"staging", "production"},
		}},
		Body: `{"answers":{"environment":"staging"}}`, WantVar: "environment",
	}, { // Test 3: An empty body still launches, which is the documented default.
		Name: "empty body", Survey: nil, Body: `{}`,
	}}

	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			store := template.NewMemStore()
			tpl := &template.Template{
				ID: "tpl_1", Name: "deploy", Tool: "bash", Command: "echo hi", Survey: test.Survey,
			}
			if err := store.Save(context.Background(), tpl); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			sub := &fakeSubmitter{run: &run.Run{ID: "run_new", Status: run.StatusPending}}
			handler := New(run.NewMemStore(), sub, zap.NewNop(), WithTemplates(store)).Handler()

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
				"/v1/templates/"+tpl.ID+"/launch", strings.NewReader(test.Body)))

			if test.WantRefused {
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("%s: status = %d, want 400: values the launch cannot use must be "+
						"refused rather than silently dropped\n%s",
						test.Name, rec.Code, rec.Body.String())
				}
				var body struct {
					Error string `json:"error"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &body)
				// A refusal has to say where the values belong, or it trades a silent drop for a
				// dead end.
				if !strings.Contains(body.Error, "extra_vars") {
					t.Errorf("%s: refusal does not say where the values go: %q", test.Name, body.Error)
				}
				if sub.gotRun != nil {
					t.Errorf("%s: a refused launch still submitted a run", test.Name)
				}
				return
			}

			if rec.Code != http.StatusCreated && rec.Code != http.StatusAccepted {
				t.Fatalf("%s: launch = %d, body %s", test.Name, rec.Code, rec.Body.String())
			}
			if sub.gotRun == nil {
				t.Fatalf("%s: no run was submitted", test.Name)
			}
			if test.WantVar != "" {
				if _, ok := sub.gotRun.ExtraVars[test.WantVar]; !ok {
					t.Errorf("%s: the run carries %v, want it to carry %q: the value the caller "+
						"passed never reached the run", test.Name, sub.gotRun.ExtraVars, test.WantVar)
				}
			}
		})
	}
}
