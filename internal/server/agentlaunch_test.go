package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/user"
)

// TestAnAgentLaunchesATemplateAsWritten pins the narrowing an agent's template launch is held to on
// the plain API, not only over MCP.
//
// The MCP server refused extra vars and a widening limit from the start, and the launch endpoint took
// both from the same agent token, so an agent that skipped MCP could rewrite what a vetted template
// does, or aim one pinned to a canary at every host, under the template name the audit trail records.
// A person keeps the latitude a launch has always had.
func TestAnAgentLaunchesATemplateAsWritten(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tokens := auth.NewMemStore()
	users := user.NewMemStore()
	templates := template.NewMemStore()

	operator, err := user.New("ops", "pw", user.RoleOperator)
	if err != nil {
		t.Fatalf("user.New: %v", err)
	}
	if err := users.Save(ctx, operator); err != nil {
		t.Fatalf("Save user: %v", err)
	}
	mint := func(name, kind string) string {
		plain, tok, err := auth.New(name)
		if err != nil {
			t.Fatalf("auth.New: %v", err)
		}
		tok.UserID, tok.Kind = operator.ID, kind
		if err := tokens.Save(ctx, tok); err != nil {
			t.Fatalf("Save token: %v", err)
		}
		return plain
	}
	agent, person := mint("deploy-bot", auth.KindAgent), mint("ops-cli", "")
	for _, tpl := range []*template.Template{
		{ID: "tpl_canary", Name: "canary deploy", Tool: run.ToolAnsible, Playbook: "site.yml",
			Inventory: "hosts.ini", Limit: "canary1", InventoryID: "inv_prod"},
		{ID: "tpl_open", Name: "deploy", Tool: run.ToolAnsible, Playbook: "site.yml",
			Inventory: "hosts.ini"},
	} {
		if err := templates.Save(ctx, tpl); err != nil {
			t.Fatalf("Save template: %v", err)
		}
	}

	tests := []struct {
		Token        string
		Template     string
		Body         string
		WantCode     int
		WantMentions string
	}{{ // Test 0: Extra vars would rewrite what the template does.
		Token: agent, Template: "tpl_open", Body: `{"extra_vars":{"env":"prod"}}`,
		WantCode: http.StatusForbidden, WantMentions: "extra_vars",
	}, { // Test 1: A pattern meaning every host widens rather than narrows.
		Token: agent, Template: "tpl_open", Body: `{"limit":"all"}`,
		WantCode: http.StatusForbidden, WantMentions: "every host",
	}, { // Test 2: So does its wildcard spelling.
		Token: agent, Template: "tpl_open", Body: `{"limit":"*"}`,
		WantCode: http.StatusForbidden, WantMentions: "every host",
	}, { // Test 3: A template that pins its target keeps it.
		Token: agent, Template: "tpl_canary", Body: `{"limit":"web01"}`,
		WantCode: http.StatusForbidden, WantMentions: "pins its target",
	}, { // Test 4: Another inventory is another target.
		Token: agent, Template: "tpl_canary", Body: `{"inventory_id":"inv_other"}`,
		WantCode: http.StatusForbidden, WantMentions: "another inventory",
	}, { // Test 5: The template's own credentials apply.
		Token: agent, Template: "tpl_open", Body: `{"credential_ids":["cred_x"]}`,
		WantCode: http.StatusForbidden, WantMentions: "credential_ids",
	}, { // Test 6: Narrowing a template that pins nothing is the useful case.
		Token: agent, Template: "tpl_open", Body: `{"limit":"web01"}`,
		WantCode: http.StatusAccepted,
	}, { // Test 7: Naming the pinned target again changes nothing.
		Token: agent, Template: "tpl_canary", Body: `{"limit":"canary1","inventory_id":"inv_prod"}`,
		WantCode: http.StatusAccepted,
	}, { // Test 8: A dry run and labels are what an agent may add.
		Token: agent, Template: "tpl_open", Body: `{"dry_run":true,"labels":{"change":"CHG0030001"}}`,
		WantCode: http.StatusAccepted,
	}, { // Test 9: A person may still reshape a launch within their grants.
		Token: person, Template: "tpl_open", Body: `{"extra_vars":{"env":"prod"},"limit":"all"}`,
		WantCode: http.StatusAccepted,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			sub := &fakeSubmitter{run: &run.Run{ID: "run_x"}}
			handler := New(run.NewMemStore(), sub, zap.NewNop(), WithTokens(tokens), WithUsers(users),
				WithTemplates(templates)).Handler()
			req := httptest.NewRequest(http.MethodPost, "/v1/templates/"+test.Template+"/launch",
				strings.NewReader(test.Body))
			req.Header.Set("Authorization", "Bearer "+test.Token)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.WantCode {
				t.Fatalf("launch = %d %s, want %d", rec.Code, rec.Body.String(), test.WantCode)
			}
			if test.WantCode == http.StatusForbidden {
				if !strings.Contains(rec.Body.String(), test.WantMentions) {
					t.Errorf("refusal %s does not mention %q", rec.Body.String(), test.WantMentions)
				}
				if sub.gotRun != nil {
					t.Error("a refused launch still reached the dispatcher")
				}
			}
		})
	}
}
