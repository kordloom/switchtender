package schedule

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// surveyTemplate is a template whose survey has every kind of question a scheduled fire must
// answer from its defaults, and, when unanswered is set, a required one it cannot.
func surveyTemplate(unanswered bool) *template.Template {
	tpl := &template.Template{
		ID: "tpl_survey", Name: "rotate db password", Playbook: "survey.yml", Inventory: "prod",
		ExtraVars: map[string]any{"marker_dir": "/tmp/marks", "db_password": "plain-template-value"},
		Survey: []template.SurveyField{
			{Var: "region", Type: template.FieldText, Default: "eu"},
			{Var: "batch", Type: template.FieldInt, Required: true, Default: float64(3)},
			{Var: "db_password", Type: template.FieldSecret, Required: true,
				SealedDefault: "sealed-default-ciphertext"},
		},
	}
	if unanswered {
		tpl.Survey = append(tpl.Survey,
			template.SurveyField{Var: "api_token", Type: template.FieldSecret, Required: true})
	}
	return tpl
}

// TestAScheduledFireCarriesTheSurveyDefaults pins what a schedule fires a surveyed template with.
//
// A schedule fired the template's extra vars and nothing else, so an optional question's default
// never reached the play, a required question's default never reached it either, and a secret
// question's sealed default stayed on the template while the play ran without the variable, or with
// a plain template value of the same name in its place.
func TestAScheduledFireCarriesTheSurveyDefaults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	templates := template.NewMemStore()
	if err := templates.Save(ctx, surveyTemplate(false)); err != nil {
		t.Fatalf("Save() template error = %v", err)
	}
	rec := &recordingSubmitter{}
	s := NewScheduler(NewMemStore(), rec, zap.NewNop(), WithTemplates(templates))
	if _, err := s.fire(ctx, &Schedule{ID: "sch_survey", TemplateID: "tpl_survey"}); err != nil {
		t.Fatalf("fire() error = %v", err)
	}
	if rec.got == nil {
		t.Fatal("fire() reached no submitter")
	}
	wantVars := map[string]any{"marker_dir": "/tmp/marks", "region": "eu", "batch": 3}
	if diff := cmp.Diff(wantVars, rec.got.ExtraVars); diff != "" {
		t.Errorf("extra vars of the scheduled run (-want +got):\n%s", diff)
	}
	wantSealed := map[string]string{"db_password": "sealed-default-ciphertext"}
	if diff := cmp.Diff(wantSealed, rec.got.SealedVars); diff != "" {
		t.Errorf("sealed vars of the scheduled run (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(run.SealedDigestsOf(wantSealed), rec.got.SealedDigests); diff != "" {
		t.Errorf("the scheduled run does not bind its sealed default (-want +got):\n%s", diff)
	}
}

// TestAScheduledFireNobodyCanAnswerIsRefusedOnTheRecord pins the refusal. A required question with
// no default has no answer when nobody is present, so the fire starts no run, the refusal is a chain
// entry of its own naming the question, and the schedule says why it fired nothing. The fire is not
// recorded as a fire, since nothing fired.
func TestAScheduledFireNobodyCanAnswerIsRefusedOnTheRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	templates := template.NewMemStore()
	if err := templates.Save(ctx, surveyTemplate(true)); err != nil {
		t.Fatalf("Save() template error = %v", err)
	}
	store := NewMemStore()
	sc := dueSchedule("sch_survey", "* * * * *")
	sc.TemplateID, sc.Playbook, sc.LastRunID = "tpl_survey", "", "run_previous"
	if err := store.Save(ctx, sc); err != nil {
		t.Fatalf("Save() schedule error = %v", err)
	}
	audits := audit.NewMemStore()
	sub := &countingKindSubmitter{}
	NewScheduler(store, sub, zap.NewNop(), WithTemplates(templates), WithAudits(audits)).
		tick(time.Now())

	if got := sub.calls.Load(); got != 0 {
		t.Errorf("the submitter was called %d times for a fire nobody could answer", got)
	}
	got, err := store.Get(ctx, "sch_survey")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	for _, want := range []string{"refused", `"api_token" is required and has no default`,
		"scheduled fire", `template "rotate db password"`} {
		if !strings.Contains(got.LastError, want) {
			t.Errorf("LastError = %q, want it to contain %q", got.LastError, want)
		}
	}
	if got.LastRunID != "run_previous" {
		t.Errorf("LastRunID = %q, want the previous run kept: the refusal created none", got.LastRunID)
	}
	entries, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("chain holds %d entries, want the refusal alone", len(entries))
	}
	e := entries[0]
	if e.Method != audit.MethodSchedule || e.Path != "/schedules/sch_survey/refused/survey/api_token" {
		t.Errorf("entry = %s %s, want SCHEDULE /schedules/sch_survey/refused/survey/api_token",
			e.Method, e.Path)
	}
	if e.Actor != "system:scheduler" || e.ContentDigest == "" {
		t.Errorf("entry actor %q digest %q, want the scheduler and a committed reason", e.Actor,
			e.ContentDigest)
	}
}

// TestARefusedFireStillRefusesWhenTheChainIsDown pins the direction a failed record errs in. The
// fire is refused either way, since there is no answer to run with, and the reason says the refusal
// could not be recorded so the gap in the trail is not silent.
func TestARefusedFireStillRefusesWhenTheChainIsDown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	templates := template.NewMemStore()
	if err := templates.Save(ctx, surveyTemplate(true)); err != nil {
		t.Fatalf("Save() template error = %v", err)
	}
	sub := &countingKindSubmitter{}
	s := NewScheduler(NewMemStore(), sub, zap.NewNop(), WithTemplates(templates),
		WithAudits(failingAudits{}))
	_, err := s.fire(ctx, &Schedule{ID: "sch_survey", TemplateID: "tpl_survey"})
	if !errors.Is(err, template.ErrUnanswered) {
		t.Fatalf("fire() error = %v, want the survey refusal", err)
	}
	if !strings.Contains(err.Error(), "could not be recorded in the audit trail") {
		t.Errorf("fire() error = %v, want it to say the refusal is missing from the trail", err)
	}
	if got := sub.calls.Load(); got != 0 {
		t.Errorf("the submitter was called %d times", got)
	}
}

// TestEveryTemplateKindAScheduleFiresGetsTheSurvey pins that a sharded template and a saved workflow
// carry the survey the same way a single run does, since a schedule fires all three.
func TestEveryTemplateKindAScheduleFiresGetsTheSurvey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		// Shape adjusts the template to the kind under test.
		Shape func(*template.Template)
		// WantKind is the submit path the fire takes.
		WantKind string
	}{{ // Test 0: A sharded template fires a split.
		Shape: func(tpl *template.Template) { tpl.Shards = 3 }, WantKind: "split",
	}, { // Test 1: A saved workflow fires a pipeline.
		Shape: func(tpl *template.Template) {
			tpl.Playbook = ""
			tpl.Steps = []run.PipelineStep{{Name: "one", Playbook: "one.yml"}}
		},
		WantKind: "pipeline",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			tpl := surveyTemplate(false)
			test.Shape(tpl)
			templates := template.NewMemStore()
			if err := templates.Save(ctx, tpl); err != nil {
				t.Fatalf("Save() template error = %v", err)
			}
			rec := &kindRecorder{}
			s := NewScheduler(NewMemStore(), rec, zap.NewNop(), WithTemplates(templates))
			if _, err := s.fire(ctx, &Schedule{ID: "sch_survey", TemplateID: "tpl_survey"}); err != nil {
				t.Fatalf("fire() error = %v", err)
			}
			if rec.kind != test.WantKind {
				t.Errorf("kind = %q, want %q", rec.kind, test.WantKind)
			}
			if diff := cmp.Diff(map[string]string{"db_password": "sealed-default-ciphertext"},
				rec.got.SealedVars, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("sealed vars (-want +got):\n%s", diff)
			}
			if rec.got.ExtraVars["region"] != "eu" {
				t.Errorf("extra vars = %v, want the survey default", rec.got.ExtraVars)
			}
		})
	}
}

// kindRecorder records which submit path a fire took and the options it carried.
type kindRecorder struct {
	// kind is single, split, or pipeline.
	kind string
	// got is a probe run with the submission's options applied.
	got *run.Run
}

// record applies opts to a probe and notes the path.
func (k *kindRecorder) record(kind string, opts []run.SubmitOption) *run.Run {
	k.kind = kind
	k.got = &run.Run{ID: "run_kind"}
	run.ApplyOptions(k.got, opts)
	return k.got
}

// Submit records a single submission.
func (k *kindRecorder) Submit(_ context.Context, _, _ string, opts ...run.SubmitOption) (*run.Run, error) {
	return k.record("single", opts), nil
}

// SubmitSplit records a split submission.
func (k *kindRecorder) SubmitSplit(_ context.Context, _, _ string, _ int, opts ...run.SubmitOption) (*run.Run, error) {
	return k.record("split", opts), nil
}

// SubmitPipeline records a pipeline submission.
func (k *kindRecorder) SubmitPipeline(_ context.Context, _, _ string, _ []run.PipelineStep,
	opts ...run.SubmitOption) (*run.Run, error) {
	return k.record("pipeline", opts), nil
}
