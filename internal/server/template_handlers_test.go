package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/util"
)

// TestLaunchTemplateCredentialSelection verifies prompt-on-launch credential selection: a chosen
// credential from the selectable set is merged onto the always-on set, a choice outside the set is
// rejected, and an empty body launches with only the always-on credentials.
func TestLaunchTemplateCredentialSelection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		Body      string
		WantCode  int
		WantCreds []string
	}{{ // Test 0: A chosen selectable credential is merged onto the fixed set.
		Name: "valid selection", Body: `{"credential_ids":["cred_a"]}`,
		WantCode: http.StatusAccepted, WantCreds: []string{"cred_fixed", "cred_a"},
	}, { // Test 1: A choice outside the selectable set is rejected.
		Name: "not selectable", Body: `{"credential_ids":["cred_x"]}`,
		WantCode: http.StatusBadRequest,
	}, { // Test 2: An empty body launches with only the always-on credentials.
		Name: "no selection", Body: "",
		WantCode: http.StatusAccepted, WantCreds: []string{"cred_fixed"},
	}}
	for i, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", i, test.Name), func(t *testing.T) {
			t.Parallel()
			store := template.NewMemStore()
			if err := store.Save(context.Background(), &template.Template{
				ID: "tpl_1", Name: "deploy", Playbook: "site.yml",
				CredentialIDs:           []string{"cred_fixed"},
				SelectableCredentialIDs: []string{"cred_a", "cred_b"},
				CreatedAt:               time.Now(),
			}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			sub := &fakeSubmitter{run: &run.Run{ID: "run_x"}}
			handler := New(run.NewMemStore(), sub, zap.NewNop(), WithTemplates(store)).Handler()
			var body io.Reader
			if test.Body != "" {
				body = strings.NewReader(test.Body)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/templates/tpl_1/launch", body)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.WantCode {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, test.WantCode, rec.Body.String())
			}
			if test.WantCode != http.StatusAccepted {
				return
			}
			if diff := cmp.Diff(test.WantCreds, sub.gotRun.CredentialIDs, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("submitted credential IDs mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestTemplateTimeoutRoundTrip covers the per-template run timeout end to end: it survives create
// and update through the API, and a launch carries it onto the submitted run so the template, not
// the server default, decides how long its work may take.
func TestTemplateTimeoutRoundTrip(t *testing.T) {
	t.Parallel()
	store := template.NewMemStore()
	sub := &fakeSubmitter{run: &run.Run{ID: "run_x"}}
	handler := New(run.NewMemStore(), sub, zap.NewNop(), WithTemplates(store)).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/templates",
		strings.NewReader(`{"name":"deploy","playbook":"site.yml","timeout":900}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var created template.Template
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created template: %v", err)
	}
	if created.Timeout != 900 {
		t.Errorf("created Timeout = %d, want 900", created.Timeout)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/templates/"+created.ID,
		strings.NewReader(`{"name":"deploy","playbook":"site.yml","timeout":120}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	stored, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if stored.Timeout != 120 {
		t.Errorf("stored Timeout after update = %d, want 120", stored.Timeout)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec,
		httptest.NewRequest(http.MethodPost, "/v1/templates/"+created.ID+"/launch", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("launch status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	if sub.gotRun.Timeout != 120 {
		t.Errorf("launched run Timeout = %d, want the template's 120", sub.gotRun.Timeout)
	}
}

// TestTemplateToolError checks template input validation per tool, including that any tool, not
// just Ansible, may pin an execution image now that the container runner plans all seven.
func TestTemplateToolError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Req  createTemplateRequest
		Want string
	}{{ // Test 0: Ansible with a playbook is valid.
		Name: "ansible ok",
		Req:  createTemplateRequest{Name: "t", Tool: "ansible", Playbook: "site.yml"},
		Want: "",
	}, { // Test 1: Ansible without a playbook is rejected.
		Name: "ansible needs playbook",
		Req:  createTemplateRequest{Name: "t", Tool: "ansible"},
		Want: "playbook is required",
	}, { // Test 2: A non-Ansible tool with a command is valid.
		Name: "bash ok",
		Req:  createTemplateRequest{Name: "t", Tool: "bash", Command: "echo hi"},
		Want: "",
	}, { // Test 3: A non-Ansible tool with an image is valid, the container gate is gone.
		Name: "terraform in a container",
		Req:  createTemplateRequest{Name: "t", Tool: "terraform", Command: "apply", Image: "ghcr.io/acme/tf:1"},
		Want: "",
	}, { // Test 4: A non-Ansible tool still needs a command.
		Name: "python needs command",
		Req:  createTemplateRequest{Name: "t", Tool: "python", Image: "ghcr.io/acme/py:3"},
		Want: "command is required for the python tool",
	}, { // Test 5: An unknown tool is rejected.
		Name: "unknown tool",
		Req:  createTemplateRequest{Name: "t", Tool: "make", Command: "all"},
		Want: "tool must be ansible, bash, terraform, opentofu, python, powershell, or go",
	}, { // Test 6: A missing name is rejected first.
		Name: "name required",
		Req:  createTemplateRequest{Tool: "ansible", Playbook: "site.yml"},
		Want: "name is required",
	}, { // Test 7: A pagerduty target carrying its routing key is valid.
		Name: "pagerduty with key",
		Req: createTemplateRequest{Name: "t", Tool: "ansible", Playbook: "site.yml",
			Notifications: []run.NotifyTarget{{Kind: run.NotifyPagerDuty, Key: "rk"}}},
		Want: "",
	}, { // Test 8: A pagerduty target with no routing key is rejected at definition, not at delivery.
		Name: "pagerduty needs key",
		Req: createTemplateRequest{Name: "t", Tool: "ansible", Playbook: "site.yml",
			Notifications: []run.NotifyTarget{{Kind: run.NotifyPagerDuty}}},
		Want: "pagerduty target needs a routing key",
	}, { // Test 9: An email target naming a recipient is valid.
		Name: "email with recipient",
		Req: createTemplateRequest{Name: "t", Tool: "ansible", Playbook: "site.yml",
			Notifications: []run.NotifyTarget{{Kind: run.NotifyEmail, To: "on@call"}}},
		Want: "",
	}, { // Test 10: An email target with no recipient is rejected.
		Name: "email needs recipient",
		Req: createTemplateRequest{Name: "t", Tool: "ansible", Playbook: "site.yml",
			Notifications: []run.NotifyTarget{{Kind: run.NotifyEmail}}},
		Want: "email target needs a recipient",
	}, { // Test 11: A grafana target needs both its annotation url and its token.
		Name: "grafana needs url and token",
		Req: createTemplateRequest{Name: "t", Tool: "ansible", Playbook: "site.yml",
			Notifications: []run.NotifyTarget{{Kind: run.NotifyGrafana, URL: "https://gf"}}},
		Want: "grafana target needs an annotation url and an api token",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := templateToolError(test.Req); got != test.Want {
				t.Errorf("templateToolError() = %q, want %q", got, test.Want)
			}
		})
	}
}

// TestNotificationURLMasking verifies notification URLs are redacted on read and that echoing a
// mask back preserves the stored address instead of overwriting it.
func TestNotificationURLMasking(t *testing.T) {
	t.Parallel()
	const secret = "https://hooks.slack.com/services/T000/B111/XXXXsecretXXXX"
	store := template.NewMemStore()
	if err := store.Save(context.Background(), &template.Template{
		ID: "tpl_1", Name: "deploy", Playbook: "site.yml",
		Notifications: []run.NotifyTarget{{Kind: "slack", URL: secret}},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithTemplates(store)).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/templates", nil))
	if strings.Contains(rec.Body.String(), "XXXXsecretXXXX") {
		t.Fatalf("list leaked the webhook secret: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hooks.slack.com") {
		t.Errorf("list dropped the channel host entirely: %s", rec.Body.String())
	}

	// Saving the masked value back must not clobber the stored URL.
	masked := util.MaskURL(secret)
	body := `{"name":"deploy","playbook":"site.yml","notifications":[{"kind":"slack","url":"` + masked + `"}]}`
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/templates/tpl_1", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d, body %s", rec.Code, rec.Body.String())
	}
	after, err := store.Get(context.Background(), "tpl_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(after.Notifications) != 1 || after.Notifications[0].URL != secret {
		t.Errorf("stored notifications = %+v, want the original secret preserved", after.Notifications)
	}
}

// TestPagerDutyKeyMaskRoundTrip proves the per-service key is redacted on read and that echoing the
// mask back on an edit preserves the stored routing key rather than overwriting it with the marker.
// A PagerDuty routing key pages the service, so it is a bearer secret exactly like a webhook URL.
func TestPagerDutyKeyMaskRoundTrip(t *testing.T) {
	t.Parallel()
	const secret = "R0UTINGKEYsecretVALUE"
	store := template.NewMemStore()
	if err := store.Save(context.Background(), &template.Template{
		ID: "tpl_pd", Name: "deploy", Playbook: "site.yml",
		Notifications: []run.NotifyTarget{{Kind: run.NotifyPagerDuty, Key: secret}},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithTemplates(store)).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/templates", nil))
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("list leaked the pagerduty routing key: %s", rec.Body.String())
	}

	// Echoing the masked key back on an edit must keep the stored key, not save the marker.
	body := `{"name":"deploy","playbook":"site.yml",` +
		`"notifications":[{"kind":"pagerduty","key":"` + util.MaskMarker + `"}]}`
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/templates/tpl_pd", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d, body %s", rec.Code, rec.Body.String())
	}
	after, err := store.Get(context.Background(), "tpl_pd")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(after.Notifications) != 1 || after.Notifications[0].Key != secret {
		t.Errorf("stored notifications = %+v, want the routing key preserved", after.Notifications)
	}
}

// TestTemplateSurveyValidatedAtSave proves a malformed survey definition is refused when the
// template is written rather than on every launch afterward.
//
// A pattern that does not compile compiled nowhere until somebody launched, so saving one produced
// a template that failed every single launch with an obscure error and no indication that the fault
// was in the definition rather than the answer.
func TestTemplateSurveyValidatedAtSave(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Survey   string
		WantCode int
	}{{ // Test 0: A well formed survey saves.
		Name:     "valid",
		Survey:   `[{"var":"env","type":"text","pattern":"^[a-z]+$"}]`,
		WantCode: http.StatusCreated,
	}, { // Test 1: An uncompilable pattern is refused at save.
		Name:     "bad pattern",
		Survey:   `[{"var":"env","type":"text","pattern":"[unclosed"}]`,
		WantCode: http.StatusBadRequest,
	}, { // Test 2: A choice field offering nothing can never be answered.
		Name:     "choice with no choices",
		Survey:   `[{"var":"env","type":"choice"}]`,
		WantCode: http.StatusBadRequest,
	}, { // Test 3: Inverted length bounds admit no answer at all.
		Name:     "inverted lengths",
		Survey:   `[{"var":"env","type":"text","min_length":9,"max_length":2}]`,
		WantCode: http.StatusBadRequest,
	}}
	for i, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", i, test.Name), func(t *testing.T) {
			t.Parallel()
			store := template.NewMemStore()
			handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_x"}},
				zap.NewNop(), WithTemplates(store)).Handler()

			body := fmt.Sprintf(`{"name":"deploy","playbook":"site.yml","survey":%s}`, test.Survey)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/templates",
				strings.NewReader(body)))
			if rec.Code != test.WantCode {
				t.Fatalf("create status = %d, want %d (body %s)",
					rec.Code, test.WantCode, rec.Body.String())
			}
			if test.WantCode != http.StatusCreated {
				return
			}

			// An update carries the same guard, or a valid template could be edited into one that
			// cannot launch.
			var created template.Template
			if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
				t.Fatalf("decode created template: %v", err)
			}
			rec = httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/templates/"+created.ID,
				strings.NewReader(
					`{"name":"deploy","playbook":"site.yml",`+
						`"survey":[{"var":"env","type":"text","pattern":"[unclosed"}]}`)))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("update with a bad pattern = %d, want 400", rec.Code)
			}
		})
	}
}

// TestLaunchCannotWriteAroundTheSurvey proves a launch may not set a survey variable through extra
// vars, which would skip the validation the survey exists to apply.
//
// Overrides merge last so a launch can add a variable the template does not set. That meant an extra
// var named after a survey field simply overwrote the answer that had just been checked, and every
// choice list, length bound, and pattern on that field was bypassable by moving the value from
// answers to extra_vars. The docs describe the survey as enforced, so this was a governance control
// that quietly did nothing.
func TestLaunchCannotWriteAroundTheSurvey(t *testing.T) {
	t.Parallel()
	store := template.NewMemStore()
	if err := store.Save(context.Background(), &template.Template{
		ID: "tpl_1", Name: "deploy", Playbook: "site.yml",
		Survey: []template.SurveyField{{
			Var: "environment", Type: template.FieldChoice,
			Choices: []string{"staging", "prod"}, Required: true,
		}},
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	sub := &fakeSubmitter{run: &run.Run{ID: "run_x"}}
	handler := New(run.NewMemStore(), sub, zap.NewNop(), WithTemplates(store)).Handler()

	launch := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
			"/v1/templates/tpl_1/launch", strings.NewReader(body)))
		return rec
	}

	// A value outside the choice list is refused when answered honestly.
	if rec := launch(`{"answers":{"environment":"production-oops"}}`); rec.Code != http.StatusBadRequest {
		t.Errorf("an answer outside the choices = %d, want 400", rec.Code)
	}
	// The same value smuggled through extra_vars must also be refused, not preferred.
	rec := launch(`{"answers":{"environment":"staging"},"extra_vars":{"environment":"production-oops"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("extra_vars overriding a survey field = %d, want 400 (body %s)",
			rec.Code, rec.Body.String())
	}
	if sub.gotRun != nil && sub.gotRun.ExtraVars["environment"] == "production-oops" {
		t.Error("the launch reached the dispatcher carrying the unvalidated value")
	}
	// An extra var the survey does not ask about is still allowed.
	if rec := launch(`{"answers":{"environment":"staging"},"extra_vars":{"note":"hello"}}`); rec.Code != http.StatusAccepted {
		t.Errorf("an unrelated extra var = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestUpdateRefusesErasingAWorkflowGraph proves an edit of a saved workflow that carries no steps
// is refused rather than silently replacing the pipeline with a single-playbook template. The
// update writes the record whole, so without the refusal any dialog save destroyed the graph and
// answered 200.
func TestUpdateRefusesErasingAWorkflowGraph(t *testing.T) {
	t.Parallel()
	store := template.NewMemStore()
	handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
		WithTemplates(store)).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/templates",
		strings.NewReader(`{"name":"rollout","steps":[{"name":"plan","tool":"bash","command":"echo plan"},{"name":"apply","tool":"bash","command":"echo apply","depends_on":["plan"]}]}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var created template.Template
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created template: %v", err)
	}

	// A steps-less edit, exactly what the single-run dialog sends, must be refused whole.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/templates/"+created.ID,
		strings.NewReader(`{"name":"rollout","playbook":"site.yml"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("steps-less update status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	stored, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(stored.Steps) != 2 {
		t.Fatalf("graph after refused edit has %d steps, want the original 2", len(stored.Steps))
	}

	// An edit that carries the full graph still goes through.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/templates/"+created.ID,
		strings.NewReader(`{"name":"rollout","steps":[{"name":"plan","tool":"bash","command":"echo plan"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("stepped update status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	stored, err = store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(stored.Steps) != 1 {
		t.Errorf("graph after stepped edit has %d steps, want 1", len(stored.Steps))
	}
}
