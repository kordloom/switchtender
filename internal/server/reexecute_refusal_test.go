package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/run"
)

// TestRetryAndRelaunchAnswerADispatcherRefusalWithItsReason pins that a retry or a relaunch the
// dispatcher refuses for a rule of its own answers the caller with the rule and the fix.
//
// Both routes submit a new run, so they meet the same refusals a submit does: a held run named by
// a path, an agent's split of a raw path, and a project commit that could not be pinned. Answered
// as a server fault, each told the one person who could act on it only that the run could not be
// retried, and left the reason in a log they cannot read.
func TestRetryAndRelaunchAnswerADispatcherRefusalWithItsReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Err is the refusal the retrier returns.
		Err error
		// WantBody is the text the answer must carry beside the refusal itself.
		WantBody string
		// WantStatus is the HTTP status the route answers with.
		WantStatus int
	}{{ // Test 0: A held run named by a path is a policy refusal.
		Err: fmt.Errorf("%w: the playbook site.yml is named by a path on this server: put it in "+
			"a project and run it from there", dispatch.ErrHeldByPath),
		WantStatus: http.StatusForbidden, WantBody: "put it in a project",
	}, { // Test 1: An agent's split of a raw inventory path is a policy refusal.
		Err:        fmt.Errorf("%w: store the inventory first", dispatch.ErrAgentPathSplit),
		WantStatus: http.StatusForbidden, WantBody: "store the inventory first",
	}, { // Test 2: A commit that could not be pinned is the remote's failure, and says to try again.
		Err:        fmt.Errorf("%w: try again: no answer", dispatch.ErrCommitUnpinned),
		WantStatus: http.StatusBadGateway, WantBody: "try again",
	}}
	routes := []string{"/v1/runs/run_1/retry", "/v1/runs/run_1/relaunch-failed"}
	for testNum, test := range tests {
		for _, route := range routes {
			t.Run(fmt.Sprintf("test %d %s", testNum, route), func(t *testing.T) {
				t.Parallel()
				store := run.NewMemStore()
				if err := store.Save(context.Background(),
					&run.Run{ID: "run_1", Tool: "ansible", Status: run.StatusFailed}); err != nil {
					t.Fatalf("Save() error = %v", err)
				}
				handler := New(store, &fakeSubmitter{}, zap.NewNop(),
					WithRetrier(&fakeRetrier{err: test.Err})).Handler()
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, route, nil))

				if rec.Code != test.WantStatus {
					t.Fatalf("status = %d, want %d: %s", rec.Code, test.WantStatus, rec.Body.String())
				}
				for _, want := range []string{test.Err.Error(), test.WantBody} {
					if !strings.Contains(rec.Body.String(), want) {
						t.Errorf("answer %q lacks %q", rec.Body.String(), want)
					}
				}
			})
		}
	}
}
