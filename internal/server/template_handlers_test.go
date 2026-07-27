package server

import (
	"context"
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
