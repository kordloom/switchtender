package dispatch

import (
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestRedactForExternalClearsOutputs proves the copy sent off the host to a webhook or plugin notifier
// carries no run Outputs. Outputs holds the values a playbook published with set_stats, which is how a
// stripped extra var, survey answer, or runtime-fetched token re-enters the run, so leaving it in
// delivered the exact secret the function takes care to clear from ExtraVars off the host in cleartext.
func TestRedactForExternalClearsOutputs(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID:            "run_1",
		ExtraVars:     map[string]any{"survey_token": "s3cr3t"},
		Outputs:       map[string]any{"deploy_token": "s3cr3t"},
		Command:       "echo s3cr3t",
		Notifications: []run.NotifyTarget{{Kind: "webhook", URL: "https://hooks.example/xyz"}},
	}
	out := redactForExternal(r)
	if out.Outputs != nil {
		t.Errorf("redacted run still carries Outputs %v: a set_stats secret leaves the host over an "+
			"external channel", out.Outputs)
	}
	// The other three the function already stripped, guarded so a future edit cannot regress them.
	if out.ExtraVars != nil || out.Command != "" || out.Notifications != nil {
		t.Errorf("redacted run kept a field it should strip: extraVars=%v command=%q notifications=%v",
			out.ExtraVars, out.Command, out.Notifications)
	}
	// The original is untouched, since only a copy leaves the host.
	if r.Outputs == nil {
		t.Error("redactForExternal mutated the caller's run")
	}
}

// TestRedactForExternalClearsStepScripts proves a pipeline's per-step scripts do not leave the host
// over an external channel. A pipeline stores each step's raw bash or python script in
// Steps[].Command, which is the same place an inline secret lands as the top-level Command, so the
// redacted copy must blank it. It also proves the caller's run is untouched, since the redacted value
// is only a copy and the copy shares the caller's Steps slice.
func TestRedactForExternalClearsStepScripts(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID:   "run_pipe",
		Kind: run.KindPipeline,
		Steps: []run.PipelineStep{
			{Name: "prepare", Tool: run.ToolBash, Command: "echo p4ssw0rd | vault write"},
			{Name: "migrate", Tool: run.ToolPython, Command: "token = 's3cr3t'"},
		},
		Notifications: []run.NotifyTarget{{Kind: "webhook", URL: "https://hooks.example/xyz"}},
	}
	out := redactForExternal(r)
	for i, s := range out.Steps {
		if s.Command != "" {
			t.Errorf("redacted step %d kept its script %q: a pipeline step secret leaves the host", i, s.Command)
		}
		if s.Name == "" {
			t.Errorf("redacted step %d lost its name: non-secret step shape should survive", i)
		}
	}
	// The caller's run keeps its scripts, since only a copy leaves the host.
	if r.Steps[0].Command == "" || r.Steps[1].Command == "" {
		t.Error("redactForExternal mutated the caller's step commands through the shared slice")
	}
}
