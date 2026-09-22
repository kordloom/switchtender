package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestADerivedRunIsScrubbedInTheResponseThatCreatedIt covers the one copy of a run that skipped the
// scrubber.
//
// Reading a run masks secret-shaped assignments in its command and launch variables for every
// caller below admin, because a run carried both verbatim and the read-only viewer role was showing
// passwords in the clear. The fix was applied to the read handlers. The write handlers return a run
// too, and they return it unscrubbed.
//
// For a submission that is merely inconsistent: the caller is handed back the command they just
// sent. For a retry or a relaunch it is the same disclosure the read path was fixed to stop, because
// those inherit the whole spec of an older run somebody else composed. An operator who may retry a
// run but may not read it unmasked gets the masked copy from GET and the unmasked one from the
// button, for the same run, in the same session.
func TestADerivedRunIsScrubbedInTheResponseThatCreatedIt(t *testing.T) {
	t.Parallel()
	const secret = "hunter2"
	derived := &run.Run{
		ID: "run_new", Tool: run.ToolBash, Status: run.StatusPending,
		Command:   "export PGPASSWORD=" + secret + " && ./migrate.sh",
		ExtraVars: map[string]any{"db_password": secret},
	}

	tests := []struct {
		Name string
		Path string
	}{
		{Name: "retry", Path: "/v1/runs/run_src/retry"},
		{Name: "relaunch", Path: "/v1/runs/run_src/relaunch-failed"},
	}

	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			users := user.NewMemStore()
			tokens := auth.NewMemStore()
			runs := run.NewMemStore()

			// An operator: allowed to retry, and below admin, so the scrub applies to them.
			operator, err := user.New("operator", "pw", user.RoleOperator)
			if err != nil {
				t.Fatalf("test %d: user.New() error = %v", testNum, err)
			}
			if err := users.Save(ctx, operator); err != nil {
				t.Fatalf("test %d: users.Save() error = %v", testNum, err)
			}
			plain, tok, err := auth.New("t-operator")
			if err != nil {
				t.Fatalf("test %d: auth.New() error = %v", testNum, err)
			}
			tok.UserID = operator.ID
			if err := tokens.Save(ctx, tok); err != nil {
				t.Fatalf("test %d: tokens.Save() error = %v", testNum, err)
			}
			three := 3
			if err := runs.Save(ctx, &run.Run{
				ID: "run_src", Tool: "ansible", Kind: run.KindSplit, ShardCount: &three,
				Status: run.StatusFailed,
			}); err != nil {
				t.Fatalf("test %d: runs.Save() error = %v", testNum, err)
			}

			handler := New(runs, &fakeSubmitter{}, zap.NewNop(), WithTokens(tokens),
				WithUsers(users), WithRetrier(&fakeRetrier{run: derived})).Handler()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, test.Path, nil)
			req.Header.Set("Authorization", "Bearer "+plain)
			handler.ServeHTTP(rec, req)

			if rec.Code >= http.StatusBadRequest {
				t.Fatalf("test %d: %s = %d: %s", testNum, test.Path, rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), secret) {
				t.Errorf("test %d: the response to %s carries the secret in the clear, while a "+
					"GET of the same run masks it for this caller:\n%s",
					testNum, test.Path, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "migrate.sh") {
				t.Errorf("test %d: the command was lost rather than masked, so an operator can no "+
					"longer read what the run does:\n%s", testNum, rec.Body.String())
			}
		})
	}
}
