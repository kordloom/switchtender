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
