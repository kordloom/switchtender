package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestReexecutingARunReauthorizesItsQueue covers the queue grant on the paths that re-run a spec.
//
// A named queue restricts a run to a worker group, which is usually a network drawn to be kept
// apart, so directing work to it is grantable: the direct launch authorizes queue:<name> with the
// same AccessUse it authorizes the project and credentials with, before it submits.
//
// Retry, relaunch and rerun re-run an existing run's spec against the same queue, and they
// authorized everything the run touched except the queue. So an operator who could reach a run,
// because they hold its project and inventory, but who holds no grant on its queue, could execute
// on that queue by clicking retry: work placed on a segment they were never allowed to direct work
// to. Reading the run stays unchanged, because the queue scopes execution rather than readability.
func TestReexecutingARunReauthorizesItsQueue(t *testing.T) {
	t.Parallel()
	paths := []struct {
		Name string
		Path string
	}{
		{Name: "retry", Path: "/v1/runs/run_q/retry"},
		{Name: "relaunch", Path: "/v1/runs/run_q/relaunch-failed"},
		{Name: "rerun", Path: "/v1/runs/run_q/rerun"},
	}

	for testNum, tc := range paths {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			users := user.NewMemStore()
			tokens := auth.NewMemStore()
			grants := grant.NewMemStore()
			runs := run.NewMemStore()

			operator, err := user.New("operator", "pw", user.RoleOperator)
			if err != nil {
				t.Fatalf("test %d: user.New() error = %v", testNum, err)
			}
			if err := users.Save(ctx, operator); err != nil {
				t.Fatalf("test %d: users.Save() error = %v", testNum, err)
			}
			plain, tok, err := auth.New("t-op")
			if err != nil {
				t.Fatalf("test %d: auth.New() error = %v", testNum, err)
			}
			tok.UserID = operator.ID
			if err := tokens.Save(ctx, tok); err != nil {
				t.Fatalf("test %d: tokens.Save() error = %v", testNum, err)
			}

			three := 3
			exit := 0
			// A finished split run on queue "prod", scoped by a project the operator will hold, so
			// the only thing standing between them and re-execution is the queue grant.
			rn := &run.Run{
				ID: "run_q", Tool: run.ToolAnsible, Kind: run.KindSplit, ShardCount: &three,
				Status: run.StatusFailed, ExitCode: &exit, Playbook: "site.yml",
				ProjectID: "prj_shared", Queue: "prod",
			}
			if err := runs.Save(ctx, rn); err != nil {
				t.Fatalf("test %d: runs.Save() error = %v", testNum, err)
			}
			// The operator can use the project, so they can reach the run. They hold no grant on
			// queue:prod.
			if err := grants.Save(ctx, &grant.Grant{
				ID: grant.NewID(), Subject: operator.ID, Object: "prj_shared", Access: grant.AccessUse,
			}); err != nil {
				t.Fatalf("test %d: grants.Save() error = %v", testNum, err)
			}

			handler := New(runs, &fakeSubmitter{run: &run.Run{ID: "run_new", Status: run.StatusPending}},
				zap.NewNop(), WithTokens(tokens),
				WithUsers(users), WithGrants(grants, true),
				WithRetrier(&fakeRetrier{run: &run.Run{ID: "run_new", Status: run.StatusPending}})).
				Handler()
			call := func() int {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, tc.Path, nil)
				req.Header.Set("Authorization", "Bearer "+plain)
				handler.ServeHTTP(rec, req)
				return rec.Code
			}

			if code := call(); code != http.StatusForbidden {
				t.Errorf("test %d: %s without a queue grant = %d, want 403: the operator directed "+
					"work to a queue they hold no grant on", testNum, tc.Path, code)
			}

			// Grant the queue. Now the re-execution is authorized.
			if err := grants.Save(ctx, &grant.Grant{
				ID: grant.NewID(), Subject: operator.ID, Object: grant.QueueObject("prod"),
				Access: grant.AccessUse,
			}); err != nil {
				t.Fatalf("test %d: grants.Save() error = %v", testNum, err)
			}
			if code := call(); code == http.StatusForbidden {
				t.Errorf("test %d: %s with a queue grant is still refused", testNum, tc.Path)
			}
		})
	}
}
