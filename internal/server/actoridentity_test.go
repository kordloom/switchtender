package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestARederivedRunCarriesTheWholeActorIdentity pins that both paths that build a new run from an
// old one pass the account and not only the name.
//
// The relaunch handed the dispatcher a name and an authentication type and nothing else, so the new
// run was stored with no account behind it. That matters because of what the account is for: the
// distinct-approver rule compares accounts when both sides have one and falls back to comparing
// credential names when either does not. One person's API token and their browser session record
// different names, so on a run with no account the fallback reports two different people and the
// person who launched it can approve it. Requiring a second pair of eyes is the entire point of the
// setting, and it was being satisfied by the same pair.
//
// The retry passed no identity at all, which cost it the approval policy as well.
func TestARederivedRunCarriesTheWholeActorIdentity(t *testing.T) {
	t.Parallel()
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

			operator, err := user.New("operator", "pw", user.RoleOperator)
			if err != nil {
				t.Fatalf("test %d: user.New() error = %v", testNum, err)
			}
			if err := users.Save(ctx, operator); err != nil {
				t.Fatalf("test %d: users.Save() error = %v", testNum, err)
			}
			// The token's name is deliberately not the account's name, which is the whole reason
			// the account has to travel separately.
			plain, tok, err := auth.New("ci-token")
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

			retrier := &fakeRetrier{run: &run.Run{ID: "run_new", Status: run.StatusPending}}
			handler := New(runs, &fakeSubmitter{}, zap.NewNop(), WithTokens(tokens),
				WithUsers(users), WithRetrier(retrier)).Handler()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, test.Path, nil)
			req.Header.Set("Authorization", "Bearer "+plain)
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK && rec.Code != http.StatusCreated &&
				rec.Code != http.StatusAccepted {
				t.Fatalf("test %d: %s = %d, want a success so the identity it passed can be "+
					"read: %s", testNum, test.Path, rec.Code, rec.Body.String())
			}
			if retrier.gotAccount != operator.ID {
				t.Errorf("test %d: the new run was given account %q, want %q. Without it the "+
					"distinct-approver rule falls back to matching credential names, and the "+
					"person who launched the run can approve it",
					testNum, retrier.gotAccount, operator.ID)
			}
			if retrier.gotActor != "ci-token" {
				t.Errorf("test %d: the new run was credited to %q, want %q",
					testNum, retrier.gotActor, "ci-token")
			}
		})
	}
}
