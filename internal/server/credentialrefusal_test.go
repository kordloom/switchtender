package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
)

// TestACredentialThatCannotOpenIsTheLaunchersToFix pins what a launch answers when the run needs a
// credential that has no secret, or one that does not open under this server's key. It was a 500
// saying "could not launch template" with the reason only in the server log. It is now a 409 that
// carries the dispatcher's words, which name the credential and the fix.
func TestACredentialThatCannotOpenIsTheLaunchersToFix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	noSecret := fmt.Errorf("%w: %q. Set its secret", credential.ErrNoSecret, "Demo Credential")
	unreadable := fmt.Errorf("%w: %q. Restore the key", credential.ErrUnreadable, "prod-ssh")
	tests := []struct {
		Err      error
		WantText string
		Path     string
		Body     string
	}{{ // Test 0: A run launched directly.
		Err: noSecret, WantText: "Demo Credential", Path: "/v1/runs", Body: `{"playbook":"hello.yml"}`,
	}, { // Test 1: A template launch.
		Err: unreadable, WantText: "prod-ssh", Path: "/v1/templates/tpl_1/launch", Body: `{}`,
	}, { // Test 2: A rerun of a finished run.
		Err: noSecret, WantText: "Demo Credential", Path: "/v1/runs/run_done/rerun",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			templates := template.NewMemStore()
			if err := templates.Save(ctx, &template.Template{
				ID: "tpl_1", Name: "Demo Job Template", Playbook: "hello.yml",
			}); err != nil {
				t.Fatalf("save template: %v", err)
			}
			runs := run.NewMemStore()
			if err := runs.Save(ctx, &run.Run{
				ID: "run_done", Playbook: "hello.yml", Status: run.StatusFailed,
			}); err != nil {
				t.Fatalf("save run: %v", err)
			}
			handler := New(runs, &fakeSubmitter{err: test.Err}, zap.NewNop(),
				WithTemplates(templates)).Handler()
			req := httptest.NewRequest(http.MethodPost, test.Path, strings.NewReader(test.Body))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusConflict {
				t.Fatalf("POST %s = %d %s, want 409", test.Path, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), test.WantText) {
				t.Errorf("POST %s answered %s, want it to name %s", test.Path, rec.Body.String(),
					test.WantText)
			}
		})
	}
}

// TestAWebhookSaysWhichCredentialIsNotReady pins the webhook's answer to the same failure. It was a
// 502 saying "could not launch the template", which sent whoever read the sender's delivery log to
// the server log to find out which credential and why.
func TestAWebhookSaysWhichCredentialIsNotReady(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	templates := template.NewMemStore()
	if err := templates.Save(ctx, &template.Template{
		ID: "tpl_1", Name: "Deploy", Playbook: "deploy.yml",
	}); err != nil {
		t.Fatalf("save template: %v", err)
	}
	triggers := trigger.NewMemStore()
	token, tg, err := trigger.New("deploy on push", "tpl_1")
	if err != nil {
		t.Fatalf("trigger.New() error = %v", err)
	}
	if err := triggers.Save(ctx, tg); err != nil {
		t.Fatalf("save trigger: %v", err)
	}
	sub := &fakeSubmitter{err: fmt.Errorf("%w: %q. Set its secret", credential.ErrNoSecret, "deploy-key")}
	handler := hookHandler(triggers, templates, sub, run.NewMemStore(), nil, nil, nil,
		newHookFlights(), nil, zap.NewNop())
	req := httptest.NewRequest(http.MethodPost, "/hooks/"+token, strings.NewReader(`{}`))
	req.SetPathValue("token", token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "deploy-key") {
		t.Errorf("webhook answered %d %s, want 409 naming the credential", rec.Code, rec.Body.String())
	}
}
