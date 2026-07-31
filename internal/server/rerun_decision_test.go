package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// TestRerunRefusesADecisionNotToRun checks that a replay cannot fire a spec somebody already
// decided should not run.
//
// A rerun carries the run's execution options and deliberately not the hold that was on it. That is
// right for a run that executed, and wrong for one that never did: a run held by require_approval,
// by a drift reconcile, or as a generated proposal is gated by nothing on replay, because only a
// stored policy is consulted on the way back in. The denied command then ran from a one-click button
// on the denied run's own page, with the approver's decision left behind in the record.
func TestRerunRefusesADecisionNotToRun(t *testing.T) {
	t.Parallel()
	started := time.Now()
	tests := []struct {
		Name       string
		In         *run.Run
		WantRefuse bool
	}{{ // Test 0: An approver denied it.
		Name: "rejected", WantRefuse: true,
		In: &run.Run{Status: run.StatusRejected},
	}, { // Test 1: Withdrawn while it waited for a decision, so it never ran.
		Name: "canceled before it started", WantRefuse: true,
		In: &run.Run{Status: run.StatusCanceled},
	}, { // Test 2: It ran and was stopped part way, which is an outcome rather than a decision.
		Name: "canceled after it started", WantRefuse: false,
		In: &run.Run{Status: run.StatusCanceled, StartedAt: &started},
	}, { // Test 3: Ordinary completions replay.
		Name: "succeeded", WantRefuse: false,
		In: &run.Run{Status: run.StatusSucceeded, StartedAt: &started},
	}, { // Test 4: A failure is the case a rerun exists for.
		Name: "failed", WantRefuse: false,
		In: &run.Run{Status: run.StatusFailed, StartedAt: &started},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := rerunRefusal(test.In)
			if test.WantRefuse && got == "" {
				t.Errorf("a %q run replays, so a decision not to run it is overridden by a rerun",
					test.In.Status)
			}
			if !test.WantRefuse && got != "" {
				t.Errorf("a %q run is refused with %q, so an ordinary rerun no longer works",
					test.In.Status, got)
			}
		})
	}
}

// TestSplitHonorsTheApprovalItWasAskedFor checks that asking for approval and asking for shards in
// the same request holds the fan-out.
//
// The hold was applied only on the single-run branch, so the combination silently got neither: the
// API answered 202 and the split ran on every host at once, past a gate the caller had asked for.
func TestSplitHonorsTheApprovalItWasAskedFor(t *testing.T) {
	t.Parallel()
	sub := &fakeSubmitter{run: &run.Run{ID: "run_1", Status: run.StatusPendingApproval}}
	handler := New(run.NewMemStore(), sub, zap.NewNop()).Handler()
	body := `{"playbook":"site.yml","inventory":"inv","shards":2,"require_approval":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if sub.gotShards != 2 {
		t.Fatalf("shards = %d, want the request to reach SubmitSplit", sub.gotShards)
	}
	if sub.gotRun == nil || sub.gotRun.Status != run.StatusPendingApproval {
		t.Errorf("split was submitted as %v, so a fan-out the caller asked to gate ran on every "+
			"host at once", sub.gotRun)
	}
}
