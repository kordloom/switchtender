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
	masked := maskNotifyURL(secret)
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
