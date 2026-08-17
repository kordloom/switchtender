package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// launchRecorder answers a template read and a launch, recording the launch body so a test can see
// exactly what the agent's proposal turned into.
type launchRecorder struct {
	// template is the JSON returned for a template read.
	template string
	// body is the decoded launch body, set when a launch arrives.
	body map[string]any
	// launched reports whether a launch reached the server at all.
	launched bool
}

// handler serves the two endpoints propose_run touches, plus the admin probe the client makes at
// startup, which must refuse so the token reads as non-admin.
func (l *launchRecorder) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/users", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	mux.HandleFunc("/v1/templates/tpl_1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, l.template)
	})
	mux.HandleFunc("/v1/templates/tpl_1/launch", func(w http.ResponseWriter, r *http.Request) {
		l.launched = true
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &l.body)
		_, _ = io.WriteString(w, `{"id":"run_1","status":"pending_approval"}`)
	})
	return mux
}

// proposeTool builds the client against a recorder and returns the propose_run tool.
func proposeTool(t *testing.T, rec *launchRecorder) Tool {
	t.Helper()
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "ymt_test", 5*time.Second)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	for _, tool := range Tools(c, Options{}) {
		if tool.Name == "propose_run" {
			return tool
		}
	}
	t.Fatal("propose_run is not in the tool set")
	return Tool{}
}

// TestProposeRunCannotWidenATemplatesTarget covers the limit argument. A template's limit is the
// operator's statement of which hosts this template may touch, and the launch endpoint takes the
// caller's limit as a replacement, not a narrowing. So an agent handed a template pinned to one
// canary host could aim the same vetted template at the entire inventory by passing a limit, and
// passing "all" did worse than widen the target: the risk grade the approval policies key on is
// computed from how wide a run reaches, so the widest possible run also graded itself down and could
// fall under the threshold that would have held it.
func TestProposeRunCannotWidenATemplatesTarget(t *testing.T) {
	t.Parallel()
	pinned := `{"id":"tpl_1","name":"canary deploy","playbook":"site.yml","limit":"canary-1"}`
	tests := []struct {
		Name     string
		Template string
		Limit    string
		WantErr  string
	}{ // Test 0: A template that pins its target refuses a different one.
		{"replaces a pinned limit", pinned, "web*", "pins"},
		// Test 1: The pattern that means every host is refused outright.
		{"all", `{"id":"tpl_1","name":"deploy","playbook":"site.yml"}`, "all", "every host"},
		// Test 2: So is the wildcard spelling of the same thing.
		{"star", `{"id":"tpl_1","name":"deploy","playbook":"site.yml"}`, "*", "every host"},
	}
	for i, tc := range tests {
		rec := &launchRecorder{template: tc.Template}
		tool := proposeTool(t, rec)
		args := json.RawMessage(`{"template_id":"tpl_1","limit":"` + tc.Limit + `"}`)
		out, err := tool.Run(context.Background(), args)
		if err == nil {
			t.Errorf("test %d (%s): propose_run(limit=%q) = %q, want a refusal",
				i, tc.Name, tc.Limit, out)
		} else if !strings.Contains(err.Error(), tc.WantErr) {
			t.Errorf("test %d (%s): error = %v, want it to mention %q", i, tc.Name, err, tc.WantErr)
		}
		if rec.launched {
			t.Errorf("test %d (%s): the run was launched despite the refusal", i, tc.Name)
		}
	}

	// A narrowing limit on a template that pins nothing is the ordinary, allowed case.
	rec := &launchRecorder{template: `{"id":"tpl_1","name":"deploy","playbook":"site.yml"}`}
	tool := proposeTool(t, rec)
	if _, err := tool.Run(context.Background(),
		json.RawMessage(`{"template_id":"tpl_1","limit":"web01"}`)); err != nil {
		t.Fatalf("propose_run with a narrowing limit = %v, want it allowed", err)
	}
	if rec.body["limit"] != "web01" {
		t.Errorf("launch body limit = %v, want web01", rec.body["limit"])
	}
}

// TestProposeRunDoesNotOverrideTemplateVariables covers extra_vars. Ansible reads extra vars at its
// highest precedence, above everything the template or the inventory sets, so an agent that can send
// them can rewrite what a vetted template does: the same template, the same name in the audit trail,
// a different target, a different version, a different action. That is the surface the ad-hoc tool is
// deliberately kept behind a flag, and extra_vars walked around it.
func TestProposeRunDoesNotOverrideTemplateVariables(t *testing.T) {
	t.Parallel()
	rec := &launchRecorder{template: `{"id":"tpl_1","name":"deploy","playbook":"site.yml"}`}
	tool := proposeTool(t, rec)
	_, err := tool.Run(context.Background(),
		json.RawMessage(`{"template_id":"tpl_1","extra_vars":{"target_env":"prod"}}`))
	if err == nil {
		t.Error("propose_run accepted agent-authored extra vars, which override the template at " +
			"Ansible's highest precedence")
	} else if !strings.Contains(err.Error(), "survey") {
		t.Errorf("error = %v, want it to point at survey answers as the supported way", err)
	}
	if rec.launched {
		t.Error("the run was launched with the agent's variables")
	}

	// Survey answers are the supported channel: the operator declared those fields, so the template
	// says which values an agent may choose.
	rec = &launchRecorder{template: `{"id":"tpl_1","name":"deploy","playbook":"site.yml"}`}
	tool = proposeTool(t, rec)
	if _, err := tool.Run(context.Background(),
		json.RawMessage(`{"template_id":"tpl_1","answers":{"env":"stage"}}`)); err != nil {
		t.Fatalf("propose_run with survey answers = %v, want it allowed", err)
	}
	if rec.body["answers"] == nil {
		t.Error("the survey answers did not reach the launch")
	}
}

// TestUnknownToolArgumentIsRefused covers a misspelled control. A model writing check_mode, the
// Ansible name, instead of dry_run had the argument silently dropped: the run executed for real, and
// the tool answered with a success the model then reported as a no-change preview. An argument the
// server does not understand is a refusal, so the mistake is visible in the one exchange where it can
// still be corrected.
func TestUnknownToolArgumentIsRefused(t *testing.T) {
	t.Parallel()
	rec := &launchRecorder{template: `{"id":"tpl_1","name":"deploy","playbook":"site.yml"}`}
	tool := proposeTool(t, rec)
	out, err := tool.Run(context.Background(),
		json.RawMessage(`{"template_id":"tpl_1","check_mode":true}`))
	if err == nil {
		t.Fatalf("propose_run(check_mode) = %q, want a refusal naming the argument", out)
	}
	if !strings.Contains(err.Error(), "check_mode") {
		t.Errorf("error = %v, want it to name the argument it did not understand", err)
	}
	if rec.launched {
		t.Error("a run executed for real from a call that meant to preview")
	}
}
