package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// TestASubmissionCannotSilentlyDropASafetyControl covers the one decode in the API that still let a
// misspelled field through.
//
// The controls on this endpoint are the safety controls: dry_run makes a change a preview,
// require_approval holds it for a person, limit confines it to one host. A non-strict decode drops a
// key it does not recognize, so a submission asking for any of them by a name off by one character was
// accepted, executed without it, and answered 202 with a body indistinguishable from the one that was
// asked for. A run meant to preview made real changes; a run meant for a canary reached the fleet.
//
// The reason recorded for leaving this decode non-strict was that the request declared no extra_vars,
// which the plugin tool contract sends. It declares them now, so the reason is gone and the last
// endpoint that could silently ignore a control is closed.
func TestASubmissionCannotSilentlyDropASafetyControl(t *testing.T) {
	t.Parallel()

	submitted := func(t *testing.T, body string) (int, *run.Run) {
		t.Helper()
		submitter := &fakeSubmitter{run: &run.Run{ID: "run_x", Status: run.StatusPending}}
		h := New(run.NewMemStore(), submitter, zap.NewNop()).Handler()
		req := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, submitter.gotRun
	}

	// Each of these is a safety control asked for by a name one character off.
	for _, test := range []struct {
		// Name says which control was misspelled.
		Name string
		// Body is the submission as the caller sent it.
		Body string
	}{
		{"dry_run", `{"tool":"bash","command":"deploy","dryrun":true}`},
		{"require_approval", `{"tool":"bash","command":"deploy","require-approval":true}`},
		{"limit", `{"tool":"ansible","playbook":"site.yml","inventory":"prod","limits":"canary-1"}`},
		{"timeout", `{"tool":"bash","command":"deploy","time_out":30}`},
	} {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			code, got := submitted(t, test.Body)
			if code != http.StatusBadRequest {
				t.Errorf("a submission misspelling %s answered %d, want %d: the control was dropped "+
					"and the run went ahead without it", test.Name, code, http.StatusBadRequest)
			}
			if got != nil {
				t.Errorf("a run was submitted despite the misspelled %s: dry_run=%v limit=%q",
					test.Name, got.DryRun, got.Limit)
			}
		})
	}

	// The controls spelled correctly still reach the run, and extra vars, whose presence was the reason
	// this decode stayed lenient, are still accepted.
	t.Run("a correct submission is unaffected", func(t *testing.T) {
		t.Parallel()
		code, got := submitted(t, `{"tool":"ansible","playbook":"site.yml","inventory":"prod",`+
			`"dry_run":true,"require_approval":true,"limit":"canary-1",`+
			`"extra_vars":{"release":"1.2.3"}}`)
		if code != http.StatusAccepted && code != http.StatusCreated {
			t.Fatalf("a valid submission answered %d", code)
		}
		if got == nil {
			t.Fatal("a valid submission reached no submitter")
		}
		if !got.DryRun {
			t.Error("dry_run did not reach the run")
		}
		if got.Limit != "canary-1" {
			t.Errorf("limit = %q, want canary-1", got.Limit)
		}
		if got.ExtraVars["release"] != "1.2.3" {
			t.Errorf("extra vars = %v, want the release the caller sent", got.ExtraVars)
		}
	})
}
