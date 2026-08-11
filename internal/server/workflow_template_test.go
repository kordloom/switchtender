package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
)

// TestWorkflowTemplateSaveValidation proves a saved workflow template is validated at write time: it
// must be a pipeline OR a single launch, never both, and its graph must be legal.
func TestWorkflowTemplateSaveValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Body     string
		WantCode int
	}{{ // Test 0: A clean workflow template saves.
		Name: "valid workflow",
		Body: `{"name":"ship","steps":[{"name":"build","tool":"bash","command":"make"},
			{"name":"deploy","playbook":"deploy.yml","depends_on":["build"]}]}`,
		WantCode: http.StatusCreated,
	}, { // Test 1: Steps plus a top-level playbook is a contradiction.
		Name:     "steps and playbook",
		Body:     `{"name":"x","playbook":"site.yml","steps":[{"name":"a","tool":"bash","command":"x"}]}`,
		WantCode: http.StatusBadRequest,
	}, { // Test 2: Steps plus shards.
		Name:     "steps and shards",
		Body:     `{"name":"x","shards":3,"steps":[{"name":"a","tool":"bash","command":"x"}]}`,
		WantCode: http.StatusBadRequest,
	}, { // Test 3: Steps plus a top-level Ansible knob that would silently do nothing.
		Name:     "steps and forks",
		Body:     `{"name":"x","forks":10,"steps":[{"name":"a","tool":"bash","command":"x"}]}`,
		WantCode: http.StatusBadRequest,
	}, { // Test 4: A cyclic graph is refused at save, not at launch.
		Name: "cycle",
		Body: `{"name":"x","steps":[{"name":"a","playbook":"a.yml","depends_on":["b"]},
			{"name":"b","playbook":"b.yml","depends_on":["a"]}]}`,
		WantCode: http.StatusBadRequest,
	}, { // Test 5: A step missing its command is refused.
		Name:     "step missing command",
		Body:     `{"name":"x","steps":[{"name":"a","tool":"bash"}]}`,
		WantCode: http.StatusBadRequest,
	}}
	for i, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			store := template.NewMemStore()
			handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_x"}},
				zap.NewNop(), WithTemplates(store)).Handler()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/templates",
				strings.NewReader(test.Body)))
			if rec.Code != test.WantCode {
				t.Fatalf("test %d: status = %d, want %d (body %s)", i, rec.Code, test.WantCode, rec.Body.String())
			}
		})
	}
}

// TestWorkflowTemplateFiresAsPipeline proves every path that fires a template routes a stepped
// template to SubmitPipeline, not Submit. A missed path would fire a workflow as an empty single run,
// which is exactly the drift the design review guarded against.
func TestWorkflowTemplateFiresAsPipeline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	steps := []run.PipelineStep{
		{Name: "build", Tool: "bash", Command: "make"},
		{Name: "deploy", Playbook: "deploy.yml", DependsOn: []string{"build"}},
	}
	newStore := func() template.Store {
		store := template.NewMemStore()
		if err := store.Save(ctx, &template.Template{
			ID: "tpl_wf", Name: "ship", Inventory: "prod", Steps: steps, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		return store
	}

	// API launch.
	t.Run("launch", func(t *testing.T) {
		t.Parallel()
		sub := &fakeSubmitter{run: &run.Run{ID: "run_x"}}
		h := New(run.NewMemStore(), sub, zap.NewNop(), WithTemplates(newStore())).Handler()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/templates/tpl_wf/launch", nil))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("launch = %d, want 202 (%s)", rec.Code, rec.Body.String())
		}
		if sub.gotSteps != 2 {
			t.Errorf("launch fired %d steps, want 2: a stepped template did not fire as a pipeline", sub.gotSteps)
		}
	})

	// Webhook trigger.
	t.Run("trigger", func(t *testing.T) {
		t.Parallel()
		sub := &fakeSubmitter{run: &run.Run{ID: "run_x"}}
		triggers := trigger.NewMemStore()
		plain, tg, err := trigger.New("wf-hook", "tpl_wf")
		if err != nil {
			t.Fatalf("trigger.New: %v", err)
		}
		if err := triggers.Save(ctx, tg); err != nil {
			t.Fatalf("save trigger: %v", err)
		}
		h := New(run.NewMemStore(), sub, zap.NewNop(),
			WithTriggers(triggers, credential.NewSealer("", "")),
			WithTemplates(newStore()), WithAudit(audit.NewMemStore())).Handler()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/hooks/"+plain, nil))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("trigger fire = %d, want 202 (%s)", rec.Code, rec.Body.String())
		}
		if sub.gotSteps != 2 {
			t.Errorf("trigger fired %d steps, want 2: a stepped template did not fire as a pipeline", sub.gotSteps)
		}
	})
}
