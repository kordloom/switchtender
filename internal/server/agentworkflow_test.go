package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/mcp"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// agentWorkflowSeed stores two saved workflows beside what agentHoldSeed stores: one whose steps
// end in a Terraform apply, and one whose Terraform step is a plan, and a person's finished
// workflow.
func agentWorkflowSeed(t *testing.T, s *agentHoldServer) {
	t.Helper()
	agentHoldSeed(t, s.DB)
	ctx := context.Background()
	for _, tpl := range []*template.Template{
		{ID: "tpl_apply", Name: "infra apply", Steps: []run.PipelineStep{
			{Name: "build", Tool: run.ToolBash, Command: "./build.sh"},
			{Name: "apply", Tool: run.ToolTerraform, Command: "infra"}}},
		{ID: "tpl_plan", Name: "infra plan", Steps: []run.PipelineStep{
			{Name: "build", Tool: run.ToolBash, Command: "./build.sh"},
			{Name: "plan", Tool: run.ToolTerraform, Command: "infra", DryRun: true}}},
	} {
		if err := s.DB.templates.Save(ctx, tpl); err != nil {
			t.Fatalf("templates.Save(%s) error = %v", tpl.ID, err)
		}
	}
	done := &run.Run{ID: "run_flow_done", Playbook: "infra apply", Kind: run.KindPipeline,
		Status: run.StatusSucceeded, CreatedAt: time.Now().Add(-time.Hour), Actor: "dev-lead-cli",
		ActorType: "token", Steps: []run.PipelineStep{
			{Name: "apply", Tool: run.ToolTerraform, Command: "infra"}}}
	if err := s.DB.runs.Save(ctx, done); err != nil {
		t.Fatalf("runs.Save(run_flow_done) error = %v", err)
	}
}

// proposeOverMCP launches the saved workflow tplID through the MCP propose_run tool with token, as
// an agent's host would, and returns the tool's answer and its error.
func proposeOverMCP(t *testing.T, s *agentHoldServer, token, tplID string) (string, error) {
	t.Helper()
	client, err := mcp.NewClient(s.URL, token, 5*time.Second)
	if err != nil {
		t.Fatalf("mcp.NewClient() error = %v", err)
	}
	for _, tool := range mcp.Tools(client, mcp.Options{}) {
		if tool.Name == "propose_run" {
			args, merr := json.Marshal(map[string]any{"template_id": tplID})
			if merr != nil {
				t.Fatalf("marshal: %v", merr)
			}
			return tool.Run(context.Background(), args)
		}
	}
	t.Fatal("the MCP server offers no propose_run tool")
	return "", nil
}

// TestAnAgentsWorkflowApplyIsRefusedOnEveryPath drives each way an agent's workflow arises: a
// workflow submitted whole, a saved workflow launched, the same launch through MCP, and a rerun of
// a finished workflow. Each is refused with a 403 saying why and what to do instead, and the
// rerun is refused as it is for anybody, since a workflow reruns only from its saved workflow. The
// controls are a person's identical request, which is accepted, the plan-only workflow, which an
// agent may still submit and which is held as before, and an exemption covering the step.
//
//nolint:funlen // Test function.
func TestAnAgentsWorkflowApplyIsRefusedOnEveryPath(t *testing.T) {
	t.Parallel()
	const whole = `{"name":"release","steps":[{"name":"build","tool":"bash","command":"./b.sh"},` +
		`{"name":"apply","tool":"terraform","command":"infra"}]}`
	const exempt = `{"name":"lead applies","effect":"exempt","tool":"terraform",` +
		`"actor":"release-agent","account":"dev-lead"}`
	tests := []struct {
		// Path and Body are the request, or MCP names the saved workflow launched over MCP.
		Path string
		// Body is the request body.
		Body string
		// MCP is the saved workflow the agent launches through propose_run, empty for none.
		MCP string
		// Person makes the request as the person.
		Person bool
		// Exemption is written by the admin first, empty for none.
		Exemption string
		// WantCode is the answer, 0 for an MCP launch, which reports by error instead.
		WantCode int
		// WantRefusal is whether the answer is the agent workflow refusal.
		WantRefusal bool
	}{{ // Test 0: A workflow submitted whole.
		Path: "/v1/pipelines", Body: whole, WantCode: http.StatusForbidden, WantRefusal: true,
	}, { // Test 1: A saved workflow launched.
		Path: "/v1/templates/tpl_apply/launch", Body: `{}`, WantCode: http.StatusForbidden,
		WantRefusal: true,
	}, { // Test 2: The same launch through the MCP tool.
		MCP: "tpl_apply", WantRefusal: true,
	}, { // Test 3: A rerun of a finished workflow, refused for anybody.
		Path: "/v1/runs/run_flow_done/rerun", Body: `{}`, WantCode: http.StatusConflict,
	}, { // Test 4: A person's workflow submitted whole is accepted, the control for test 0.
		Path: "/v1/pipelines", Body: whole, Person: true, WantCode: http.StatusAccepted,
	}, { // Test 5: A person's launch is accepted, the control for test 1.
		Path: "/v1/templates/tpl_apply/launch", Body: `{}`, Person: true,
		WantCode: http.StatusAccepted,
	}, { // Test 6: An agent's plan-only workflow is accepted and held, as before.
		Path: "/v1/templates/tpl_plan/launch", Body: `{}`, WantCode: http.StatusAccepted,
	}, { // Test 7: The same plan-only workflow through MCP is accepted.
		MCP: "tpl_plan",
	}, { // Test 8: An exemption covering the apply step lets the agent submit the workflow.
		Path: "/v1/pipelines", Body: whole, Exemption: exempt, WantCode: http.StatusAccepted,
	}}
	for _, backend := range agentHoldBackends() {
		for testNum, test := range tests {
			t.Run(fmt.Sprintf("%s test %d", backend.Name, testNum), func(t *testing.T) {
				t.Parallel()
				s := newAgentHoldServer(t, backend.Open(t), true)
				agentWorkflowSeed(t, s)
				if test.Exemption != "" {
					if code, got := s.call(s.Admin, http.MethodPost, "/v1/policies",
						test.Exemption); code != http.StatusCreated {
						t.Fatalf("admin writing the exemption = %d: %s", code, got)
					}
				}
				token := s.Agent
				if test.Person {
					token = s.Person
				}
				var body string
				if test.MCP != "" {
					reply, err := proposeOverMCP(t, s, token, test.MCP)
					if refused := err != nil; refused != test.WantRefusal {
						t.Fatalf("propose_run refused = %v (%v), want %v: %s", refused, err,
							test.WantRefusal, reply)
					}
					if err != nil {
						body = err.Error()
					}
				} else {
					var code int
					code, body = s.call(token, http.MethodPost, test.Path, test.Body)
					if code != test.WantCode {
						t.Fatalf("POST %s = %d, want %d: %s", test.Path, code, test.WantCode, body)
					}
				}
				if !test.WantRefusal {
					return
				}
				for _, want := range []string{"may not apply Terraform", "as its own run",
					"effect exempt"} {
					if !strings.Contains(body, want) {
						t.Errorf("the refusal %q does not say %q", body, want)
					}
				}
			})
		}
	}
}
