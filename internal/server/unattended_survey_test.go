package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
)

// surveyHook is a server with one push trigger firing one surveyed template.
type surveyHook struct {
	// handler serves the API and the hook.
	handler http.Handler
	// templates holds the template, so a test can change its survey between deliveries.
	templates template.Store
	// triggers holds the trigger, so a test can read what it recorded.
	triggers trigger.Store
	// audits records the chain.
	audits *recordingAudits
	// token is the hook's token.
	token string
	// triggerID is the trigger's id.
	triggerID string
}

// newSurveyHook builds the server around tpl and sub.
func newSurveyHook(t *testing.T, tpl *template.Template, sub *fakeSubmitter) *surveyHook {
	t.Helper()
	ctx := context.Background()
	h := &surveyHook{
		templates: template.NewMemStore(), triggers: trigger.NewMemStore(),
		audits: &recordingAudits{sub: sub},
	}
	if err := h.templates.Save(ctx, tpl); err != nil {
		t.Fatalf("save template: %v", err)
	}
	plain, tg, err := trigger.New("on push", tpl.ID)
	if err != nil {
		t.Fatalf("trigger.New: %v", err)
	}
	if err := h.triggers.Save(ctx, tg); err != nil {
		t.Fatalf("save trigger: %v", err)
	}
	h.token, h.triggerID = plain, tg.ID
	h.handler = New(run.NewMemStore(), sub, zap.NewNop(),
		WithTriggers(h.triggers, credential.NewSealer("", "")),
		WithTemplates(h.templates), WithAudit(h.audits)).Handler()
	return h
}

// deliver posts one webhook delivery, each with its own delivery id so none is read as a repeat.
func (h *surveyHook) deliver(id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/hooks/"+h.token, nil)
	req.Header.Set("X-GitHub-Delivery", id)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// trigger reads the trigger back.
func (h *surveyHook) trigger(t *testing.T) *trigger.Trigger {
	t.Helper()
	tg, err := h.triggers.Get(context.Background(), h.triggerID)
	if err != nil {
		t.Fatalf("Get trigger: %v", err)
	}
	return tg
}

// TestAWebhookFireCarriesTheSurveyDefaults pins what a webhook fires a surveyed template with: the
// template's own vars, every question's default, and a secret question's sealed default, still
// sealed. A webhook fired the template's extra vars alone, so none of the survey reached the play.
func TestAWebhookFireCarriesTheSurveyDefaults(t *testing.T) {
	t.Parallel()
	sub := &fakeSubmitter{run: &run.Run{ID: "run_hook", Status: run.StatusPending}}
	h := newSurveyHook(t, &template.Template{
		ID: "tpl_s", Name: "deploy", Playbook: "site.yml",
		ExtraVars: map[string]any{"env": "prod", "db_password": "plain-template-value"},
		Survey: []template.SurveyField{
			{Var: "region", Type: template.FieldText, Default: "eu"},
			{Var: "batch", Type: template.FieldInt, Required: true, Default: float64(2)},
			{Var: "db_password", Type: template.FieldSecret, SealedDefault: "sealed-default"},
		},
	}, sub)
	if rec := h.deliver("d-1"); rec.Code != http.StatusAccepted {
		t.Fatalf("delivery = %d %s, want 202", rec.Code, rec.Body.String())
	}
	if sub.gotRun == nil {
		t.Fatal("the delivery launched nothing")
	}
	if diff := cmp.Diff(map[string]any{"env": "prod", "region": "eu", "batch": 2},
		sub.gotRun.ExtraVars); diff != "" {
		t.Errorf("extra vars (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(map[string]string{"db_password": "sealed-default"},
		sub.gotRun.SealedVars); diff != "" {
		t.Errorf("sealed vars (-want +got):\n%s", diff)
	}
}

// TestAWebhookNobodyCanAnswerIsRefusedOnTheRecord pins the refusal. A required question with no
// default has no answer when a webhook fires, so no run starts: the sender is told why with a 409,
// the chain records the refusal in place of a fire, naming the question, and the trigger keeps the
// reason until a later delivery fires, which clears it.
func TestAWebhookNobodyCanAnswerIsRefusedOnTheRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sub := &fakeSubmitter{run: &run.Run{ID: "run_hook", Status: run.StatusPending}}
	tpl := &template.Template{
		ID: "tpl_s", Name: "rotate", Playbook: "site.yml",
		Survey: []template.SurveyField{{Var: "db_password", Type: template.FieldSecret, Required: true}},
	}
	h := newSurveyHook(t, tpl, sub)
	rec := h.deliver("d-1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("delivery = %d %s, want 409", rec.Code, rec.Body.String())
	}
	for _, want := range []string{`\"db_password\" is required and has no default`, "webhook fire",
		`template \"rotate\"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the sender was told %s, want it to contain %s", rec.Body.String(), want)
		}
	}
	if sub.gotRun != nil {
		t.Errorf("a run was submitted with a required answer missing: %+v", sub.gotRun)
	}
	wantPath := "/hooks/" + h.triggerID + "/refused/survey/db_password"
	if len(h.audits.entries) != 1 || h.audits.entries[0].Path != wantPath {
		t.Fatalf("chain = %+v, want the one refusal entry at %s", h.audits.entries, wantPath)
	}
	if rec.Header().Get(AuditReceiptHeader) == "" {
		t.Error("the refused delivery carries no receipt for the refusal entry")
	}
	tg := h.trigger(t)
	if !strings.Contains(tg.LastError, `"db_password" is required`) || tg.LastErrorAt == nil {
		t.Errorf("trigger last_error = %q at %v, want the refusal", tg.LastError, tg.LastErrorAt)
	}
	if tg.LastFiredAt != nil {
		t.Errorf("a refused delivery stamped a fire time %v", tg.LastFiredAt)
	}

	// Give the question a default and the next delivery fires with it, and clears the refusal.
	tpl.Survey[0].SealedDefault = "sealed-default"
	if err := h.templates.Update(ctx, tpl); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if rec := h.deliver("d-2"); rec.Code != http.StatusAccepted {
		t.Fatalf("delivery after a default = %d %s, want 202", rec.Code, rec.Body.String())
	}
	if diff := cmp.Diff(map[string]string{"db_password": "sealed-default"},
		sub.gotRun.SealedVars); diff != "" {
		t.Errorf("sealed vars (-want +got):\n%s", diff)
	}
	if tg := h.trigger(t); tg.LastError != "" || tg.LastErrorAt != nil || tg.LastFiredAt == nil {
		t.Errorf("after a fire: last_error=%q at %v fired %v, want the refusal cleared",
			tg.LastError, tg.LastErrorAt, tg.LastFiredAt)
	}
}

// TestAWebhookThatStartsNoRunSaysWhyOnTheTrigger pins the other reasons a delivery starts no run.
// The trigger's last error is what an operator reads to learn why a webhook stopped launching, so it
// says why whatever stopped it, not only for a survey.
func TestAWebhookThatStartsNoRunSaysWhyOnTheTrigger(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// SubmitErr is what the submitter answers.
		SubmitErr error
		// WantStatus is what the sender is told.
		WantStatus int
		// WantError is a fragment of the trigger's last error.
		WantError string
	}{{ // Test 0: A rule refuses the run.
		SubmitErr:  fmt.Errorf("%w: production freeze", dispatch.ErrPolicyDenied),
		WantStatus: http.StatusForbidden, WantError: "production freeze",
	}, { // Test 1: A credential has no secret yet.
		SubmitErr:  fmt.Errorf("%w: deploy key", credential.ErrNoSecret),
		WantStatus: http.StatusConflict, WantError: "deploy key",
	}, { // Test 2: The launch fails for a reason the sender is not shown.
		SubmitErr:  errors.New("disk full"),
		WantStatus: http.StatusBadGateway, WantError: "could not launch the template",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			sub := &fakeSubmitter{err: test.SubmitErr}
			h := newSurveyHook(t, &template.Template{ID: "tpl_s", Name: "deploy",
				Playbook: "site.yml"}, sub)
			if rec := h.deliver("d-1"); rec.Code != test.WantStatus {
				t.Fatalf("delivery = %d %s, want %d", rec.Code, rec.Body.String(), test.WantStatus)
			}
			if tg := h.trigger(t); !strings.Contains(tg.LastError, test.WantError) {
				t.Errorf("trigger last_error = %q, want it to contain %q", tg.LastError,
					test.WantError)
			}
		})
	}
}

// TestAReviewPlanNobodyCanAnswerIsRefused pins the pull request path. A plan is launched by a
// webhook with nobody present to answer the template's survey, so a required question with no
// default refuses it: no run, a refusal on the chain naming the question, the pull request told,
// and the trigger noting it. Given a default, a redelivery of the same event plans with it.
func TestAReviewPlanNobodyCanAnswerIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rs := newReviewServer(t, reviewSetup{})
	tpl, err := rs.srv.templates.Get(ctx, "tpl_net")
	if err != nil {
		t.Fatalf("Get template: %v", err)
	}
	tpl.Survey = []template.SurveyField{{Var: "region", Type: template.FieldText, Required: true}}
	if err := rs.srv.templates.Update(ctx, tpl); err != nil {
		t.Fatalf("Update template: %v", err)
	}
	rec := rs.fire(t, rs.eventName(), rs.payload(rs.opened(), rs.headSHA, ""), "")
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "refused") {
		t.Fatalf("webhook = %d %s, want 202 refused", rec.Code, rec.Body.String())
	}
	rs.waitStatus(t, rs.headSHA, "error")
	rs.srv.reviews.Wait()
	list, err := rs.runs.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 0 {
		t.Errorf("a plan nobody could answer launched %d runs", len(list))
	}
	if !chainHasPath(t, rs.audits, "/hooks/"+rs.triggerID+"/review/7/refused/survey/region") {
		t.Error("the refusal is not on the chain")
	}
	if c := rs.forge.Comments(7); len(c) != 1 || !strings.Contains(c[0].Body, `"region" is required`) {
		t.Errorf("comments = %+v, want one naming the unanswered question", c)
	}
	tg, err := rs.srv.triggers.Get(ctx, rs.triggerID)
	if err != nil {
		t.Fatalf("Get trigger: %v", err)
	}
	if !strings.Contains(tg.LastError, `"region" is required`) {
		t.Errorf("trigger last_error = %q, want the refusal", tg.LastError)
	}

	// Once the question has a default, the forge redelivering the same event, which is how a
	// refused delivery is retried, plans the pull request with the default.
	tpl.Survey[0].Default = "eu-west"
	if err := rs.srv.templates.Update(ctx, tpl); err != nil {
		t.Fatalf("Update template: %v", err)
	}
	rec = rs.fire(t, rs.eventName(), rs.payload(rs.opened(), rs.headSHA, ""), "")
	plan := rs.planRun(t, rec)
	if plan.ExtraVars["region"] != "eu-west" || !plan.DryRun {
		t.Errorf("plan vars %v dry run %v, want the default and a plan", plan.ExtraVars, plan.DryRun)
	}
	rs.waitStatus(t, rs.headSHA, successState)
}

// TestACallbackTakesARequiredQuestionsDefault pins the callback path to the same rule as the other
// launches nobody answers. A required question with a default has an answer, its default, and a
// callback refused it as if it had none, while a schedule or a webhook of the same template ran.
func TestACallbackTakesARequiredQuestionsDefault(t *testing.T) {
	t.Parallel()
	sub := &fakeSubmitter{run: &run.Run{ID: "run_cb", Status: run.StatusPending}}
	f := newCallbackFixture(t, sub, fakeResolver{}, &template.Template{ID: "tpl_cb", Name: "boot",
		Playbook: "boot.yml", InventoryID: "inv_1", AllowCallbacks: true, CreatedAt: time.Now(),
		ExtraVars: map[string]any{"db_password": "plain-template-value"},
		Survey: []template.SurveyField{
			{Var: "role", Type: template.FieldText, Required: true, Default: "web"},
			{Var: "db_password", Type: template.FieldSecret, Required: true, SealedDefault: "sealed"},
		}})
	key := f.mintKey(t, "tpl_cb", nil)
	if rec := f.callBack("tpl_cb", "10.0.0.11", "application/json", keyBody(key)); rec.Code !=
		http.StatusCreated {
		t.Fatalf("callback = %d %s, want 201", rec.Code, rec.Body.String())
	}
	if diff := cmp.Diff(map[string]any{"role": "web"}, sub.gotRun.ExtraVars); diff != "" {
		t.Errorf("extra vars (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(map[string]string{"db_password": "sealed"}, sub.gotRun.SealedVars); diff != "" {
		t.Errorf("sealed vars (-want +got):\n%s", diff)
	}
}

// TestALaunchLeavesNoPlainValueBesideASecretAnswer pins the interactive launch against the case
// run.WithExtraVars could not express. A template whose only extra var shares its name with a secret
// question leaves no plain variable once the answer is sealed, and an empty set handed to
// WithExtraVars kept the template's own value, so the plain value sat on the run, in every read and
// receipt, beside the sealed answer it was meant to give way to.
func TestALaunchLeavesNoPlainValueBesideASecretAnswer(t *testing.T) {
	t.Parallel()
	templates := template.NewMemStore()
	if err := templates.Save(context.Background(), &template.Template{
		ID: "tpl_1", Name: "rotate", Tool: "bash", Command: "rotate",
		ExtraVars: map[string]any{"db_password": "plain-template-value"},
		Survey:    []template.SurveyField{{Var: "db_password", Type: template.FieldSecret, Required: true}},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	sub := &fakeSubmitter{run: &run.Run{ID: "run_new", Status: run.StatusPending}}
	handler := New(run.NewMemStore(), sub, zap.NewNop(), WithTemplates(templates),
		WithCredentials(credential.NewMemStore(), secretSurveySealer())).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/templates/tpl_1/launch",
		strings.NewReader(`{"answers":{"db_password":"the-launch-answer"}}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("launch = %d %s", rec.Code, rec.Body.String())
	}
	if diff := cmp.Diff(map[string]any{}, sub.gotRun.ExtraVars, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the run carries a plain value beside the sealed answer (-want +got):\n%s", diff)
	}
	if len(sub.gotRun.SealedVars) != 1 || len(sub.gotRun.SealedDigests) != 1 {
		t.Errorf("sealed vars %v digests %v, want the answer sealed and bound", sub.gotRun.SealedVars,
			sub.gotRun.SealedDigests)
	}
}

// recordedPaths returns the paths of the entries a recordingAudits holds.
func recordedPaths(a *recordingAudits) []string {
	out := make([]string, 0, len(a.entries))
	for _, e := range a.entries {
		out = append(out, e.Path)
	}
	return out
}

// TestARefusedWebhookFailsClosedWhenTheChainIsDown pins the ordering every webhook record keeps: a
// refusal that cannot be recorded is answered 503, never as a fire, and nothing is submitted.
func TestARefusedWebhookFailsClosedWhenTheChainIsDown(t *testing.T) {
	t.Parallel()
	sub := &fakeSubmitter{run: &run.Run{ID: "run_hook", Status: run.StatusPending}}
	h := newSurveyHook(t, &template.Template{
		ID: "tpl_s", Name: "rotate", Playbook: "site.yml",
		Survey: []template.SurveyField{{Var: "release", Type: template.FieldText, Required: true}},
	}, sub)
	h.audits.err = errors.New("chain unavailable")
	if rec := h.deliver("d-1"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("delivery = %d %s, want 503", rec.Code, rec.Body.String())
	}
	if sub.gotRun != nil || len(recordedPaths(h.audits)) != 0 {
		t.Errorf("submitted %+v, recorded %v, want neither", sub.gotRun, recordedPaths(h.audits))
	}
	if tg := h.trigger(t); !strings.Contains(tg.LastError, "could not be recorded") {
		t.Errorf("trigger last_error = %q, want the unrecorded refusal noted", tg.LastError)
	}
}

// TestDoctorNamesAScheduleNobodyCanAnswer pins the warning that comes before the first refusal. A
// schedule whose template's survey has a required question with no default refuses every fire, and
// without this the first sign was a fire that came due and ran nothing. An enabled schedule is
// broken; a disabled one is a warning about what enabling it would do.
func TestDoctorNamesAScheduleNobodyCanAnswer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tpls := template.NewMemStore()
	for _, tpl := range []*template.Template{{
		ID: "tpl_open", Name: "rotate", Playbook: "site.yml",
		Survey: []template.SurveyField{{Var: "db_password", Type: template.FieldSecret, Required: true}},
	}, {
		ID: "tpl_fine", Name: "deploy", Playbook: "site.yml",
		Survey: []template.SurveyField{{Var: "region", Type: template.FieldText, Required: true,
			Default: "eu"}},
	}} {
		if err := tpls.Save(ctx, tpl); err != nil {
			t.Fatalf("Save(template) error = %v", err)
		}
	}
	scheds := schedule.NewMemStore()
	for _, sc := range []*schedule.Schedule{
		{ID: "sch_on", Name: "nightly rotate", Cron: "0 2 * * *", TemplateID: "tpl_open", Enabled: true},
		{ID: "sch_off", Name: "paused rotate", Cron: "0 3 * * *", TemplateID: "tpl_open"},
		{ID: "sch_fine", Name: "nightly deploy", Cron: "0 4 * * *", TemplateID: "tpl_fine", Enabled: true},
	} {
		if err := scheds.Save(ctx, sc); err != nil {
			t.Fatalf("Save(schedule) error = %v", err)
		}
	}
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), WithTemplates(tpls),
		WithSchedules(scheds)).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/doctor", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("doctor = %d, body %s", rec.Code, rec.Body.String())
	}
	var report doctorReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[string]string{}
	for _, f := range report.Findings {
		if f.ObjectType == "schedule" && strings.Contains(f.Problem, "db_password") {
			got[f.ObjectID] = f.Severity
		}
	}
	if diff := cmp.Diff(map[string]string{"sch_on": "broken", "sch_off": "warning"}, got); diff != "" {
		t.Errorf("schedules named for an unanswerable survey (-want +got):\n%s", diff)
	}
}
