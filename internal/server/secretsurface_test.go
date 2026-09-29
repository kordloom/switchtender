package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/user"
)

// TestNoReadSurfaceShowsAViewerASecret sweeps the read surface instead of one endpoint at a time.
//
// Masking a run's inline secrets was applied endpoint by endpoint, and that is how it kept being
// incomplete. The read handlers scrubbed and the write handlers did not. When the write handlers
// were fixed, the change view was still handing over raw store rows, the reconcile proposal was
// still going out unmasked, the scrub itself never looked at a pipeline step's script, and a
// template served the same command bytes the run beside it was hiding. Each was found separately,
// after the previous one was called done.
//
// So the assertion here is not about a handler. One run and one template are planted with a secret
// in every field that can hold one, and every read a viewer can reach is required not to contain
// it. A new endpoint that forgets fails this without anybody remembering to add it.
func TestNoReadSurfaceShowsAViewerASecret(t *testing.T) {
	t.Parallel()
	const secret = "hunter2-do-not-disclose"
	ctx := context.Background()

	users := user.NewMemStore()
	tokens := auth.NewMemStore()
	runs := run.NewMemStore()
	templates := template.NewMemStore()

	viewer, err := user.New("viewer", "pw", user.RoleViewer)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	if err := users.Save(ctx, viewer); err != nil {
		t.Fatalf("users.Save() error = %v", err)
	}
	plain, tok, err := auth.New("t-viewer")
	if err != nil {
		t.Fatalf("auth.New() error = %v", err)
	}
	tok.UserID = viewer.ID
	if err := tokens.Save(ctx, tok); err != nil {
		t.Fatalf("tokens.Save() error = %v", err)
	}

	started := time.Now().Add(-time.Hour)
	ended := started.Add(time.Minute)
	// Every field that can carry an inline secret carries one: the command, a launch variable, a
	// nested launch variable, and a pipeline step's own script.
	planted := &run.Run{
		ID: "run_secret", Tool: run.ToolBash, Status: run.StatusSucceeded,
		Kind:      run.KindPipeline,
		Command:   "export PGPASSWORD=" + secret + " && ./migrate.sh",
		ExtraVars: map[string]any{"db_password": secret, "deep": map[string]any{"api_key": secret}},
		Steps: []run.PipelineStep{
			{Name: "one", Tool: run.ToolBash, Command: "TOKEN=" + secret + " ./step.sh"},
		},
		Labels: map[string]string{run.ChangeLabel: "CHG-1"}, StartedAt: &started, EndedAt: &ended, CreatedAt: started,
	}
	if err := runs.Save(ctx, planted); err != nil {
		t.Fatalf("runs.Save() error = %v", err)
	}
	// A baseline, so the comparison read below has something to compare against. Without it the
	// handler answers 404 for want of an earlier run and the path excused itself from the battery,
	// which left the one read that renders two runs at once unchecked for disclosure. It carries the
	// secret in the same places, because a comparison that leaked only the baseline's copy would
	// have passed on a fixture that planted the secret in just one side.
	earlier := started.Add(-time.Hour)
	baseline := &run.Run{
		ID: "run_baseline", Tool: run.ToolBash, Status: run.StatusSucceeded,
		Kind:      run.KindPipeline,
		Command:   "export PGPASSWORD=" + secret + " && ./migrate.sh",
		ExtraVars: map[string]any{"db_password": secret, "deep": map[string]any{"api_key": secret}},
		Steps: []run.PipelineStep{
			{Name: "one", Tool: run.ToolBash, Command: "TOKEN=" + secret + " ./step.sh"},
		},
		StartedAt: &earlier, EndedAt: &started, CreatedAt: earlier,
	}
	if err := runs.Save(ctx, baseline); err != nil {
		t.Fatalf("runs.Save() baseline error = %v", err)
	}
	if err := templates.Save(ctx, &template.Template{
		ID: "tpl_secret", Name: "nightly", Tool: run.ToolBash,
		Command:   "export PGPASSWORD=" + secret + " && ./nightly.sh",
		ExtraVars: map[string]any{"db_password": secret},
		Steps: []run.PipelineStep{
			{Name: "one", Tool: run.ToolBash, Command: "TOKEN=" + secret + " ./step.sh"},
		},
	}); err != nil {
		t.Fatalf("templates.Save() error = %v", err)
	}

	handler := New(runs, &fakeSubmitter{}, zap.NewNop(), WithTokens(tokens), WithUsers(users),
		WithTemplates(templates)).Handler()

	// Every read a viewer can reach that could carry a run or a template.
	paths := []string{
		"/v1/runs",
		"/v1/runs/run_secret",
		"/v1/runs/run_secret/shards",
		"/v1/runs/run_secret/steps",
		"/v1/runs/run_secret/compare",
		"/v1/changes",
		"/v1/changes/CHG-1",
		"/v1/templates",
		"/v1/tasks",
		"/v1/fleet",
		"/v1/estate",
		"/v1/drift",
		"/v1/hosts/host1/runs",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer "+plain)
			handler.ServeHTTP(rec, req)

			// A path listed here is a read a viewer can reach, so one that cannot be reached is
			// either a stale entry or a fixture that stopped building what the route needs. Both
			// are worth knowing. Standing down instead left the comparison read, the one that
			// renders two runs at once, excused from the battery for want of a baseline run, and
			// the list said it was covered.
			if rec.Code == http.StatusNotFound || rec.Code == http.StatusNotImplemented {
				t.Fatalf("%s answered %d, so this read was never checked for disclosure. Either "+
					"the path has moved and this list is stale, or the fixture no longer builds "+
					"what the route needs.", path, rec.Code)
			}
			if strings.Contains(rec.Body.String(), secret) {
				t.Errorf("%s shows a viewer the secret in the clear.\nThe run scrub exists so "+
					"the read-only role an outside auditor is given cannot read a password out "+
					"of a command. Body:\n%s", path, rec.Body.String())
			}
		})
	}
}
