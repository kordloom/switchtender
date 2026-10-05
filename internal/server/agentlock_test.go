package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestTheDoorAloneStopsAnAgentApprovingAHeldRun proves the first lock on a held run's approval
// stands without the second. The approver behind the route here is a stub with no agent check of
// its own, the shape of a dispatcher that lost its lock, so the only thing between an agent's token
// and a release is the gate capping that token below the role the route needs. The token is bound
// to an admin account, the strongest case: the cap must hold whatever account sits behind it.
//
// The person's token is the negative control. It reaches the same stub and releases the run, so the
// stub would have approved anything the door let through.
func TestTheDoorAloneStopsAnAgentApprovingAHeldRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Agent mints the caller's token as an agent's.
		Agent bool
		// WantCode is the response status.
		WantCode int
		// WantCalled is how many decisions reached the approver.
		WantCalled int
	}{{ // Test 0: The agent is refused at the door and the approver never hears of it.
		Agent: true, WantCode: http.StatusForbidden, WantCalled: 0,
	}, { // Test 1: A person's token on the same account reaches the approver, which releases.
		Agent: false, WantCode: http.StatusOK, WantCalled: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			tokens, users := auth.NewMemStore(), user.NewMemStore()
			admin, err := user.New("ops-admin", "a long enough password", user.RoleAdmin)
			if err != nil {
				t.Fatalf("user.New() error = %v", err)
			}
			if err := users.Save(ctx, admin); err != nil {
				t.Fatalf("users.Save() error = %v", err)
			}
			plain, tok, err := auth.New("deploy-bot")
			if err != nil {
				t.Fatalf("auth.New() error = %v", err)
			}
			tok.UserID = admin.ID
			if test.Agent {
				tok.Kind = auth.KindAgent
			}
			if err := tokens.Save(ctx, tok); err != nil {
				t.Fatalf("tokens.Save() error = %v", err)
			}
			store := run.NewMemStore()
			held := heldRun(t, false)
			if err := store.Save(ctx, held); err != nil {
				t.Fatalf("store.Save() error = %v", err)
			}
			released := held.Clone()
			released.Status = run.StatusPending
			stub := &stubApprover{result: released}
			handler := New(store, &fakeSubmitter{}, zap.NewNop(), WithTokens(tokens),
				WithUsers(users), WithAudit(audit.NewMemStore()), WithApprover(stub)).Handler()
			req := httptest.NewRequest(http.MethodPost, "/v1/runs/"+held.ID+"/approve", nil)
			req.Header.Set("Authorization", "Bearer "+plain)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if diff := cmp.Diff(test.WantCode, rec.Code); diff != "" {
				t.Errorf("status mismatch (-want +got):\n%s\nbody: %s", diff, rec.Body.String())
			}
			if diff := cmp.Diff(test.WantCalled, stub.called); diff != "" {
				t.Errorf("decisions reaching the approver mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestTheDispatcherAloneStopsAnAgentApprovingAHeldRun proves the second lock stands without the
// first. The handler is called directly, with no gate in front of it, carrying an agent's identity
// at the admin role: the shape of a token that somehow reached the route. The real dispatcher sits
// behind it, and its own check is the only thing left to stop the release.
//
// The person's identity is the negative control: the same direct call releases the run, so the
// refusal is the dispatcher's agent lock and not the missing gate.
func TestTheDispatcherAloneStopsAnAgentApprovingAHeldRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Actor is the identity the request carries past the missing gate.
		Actor Actor
		// Recorded is how the request is attributed, which is what the decision names.
		Recorded recordedActor
		// WantCode is the response status.
		WantCode int
		// WantStatus is the run's status afterward.
		WantStatus run.Status
		// WantBody is text the response must carry.
		WantBody string
	}{{ // Test 0: An agent at the admin role is refused by the dispatcher and the run stays held.
		Actor: Actor{UserID: "user_admin", Role: user.RoleAdmin, Name: "deploy-bot", Agent: true,
			Type: actorTypeAgent},
		Recorded: recordedActor{Name: "deploy-bot", Type: actorTypeAgent, OnBehalfOf: "ops-admin"},
		WantCode: http.StatusForbidden, WantStatus: run.StatusPendingApproval,
		WantBody: "an agent cannot approve",
	}, { // Test 1: A person's session through the same direct call releases the run.
		Actor: Actor{UserID: "user_admin", Role: user.RoleAdmin, Name: "ops-admin",
			Type: actorTypeSession},
		Recorded: recordedActor{Name: "ops-admin", Type: actorTypeSession, OnBehalfOf: "ops-admin"},
		WantCode: http.StatusOK, WantStatus: run.StatusPending, WantBody: `"status":"pending"`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			audits := audit.NewMemStore()
			runner := roundhouse.RunnerFunc(
				func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
					return roundhouse.Result{ExitCode: 0}, nil
				},
			)
			d := dispatch.New(store, runner, zap.NewNop(), dispatch.WithAudits(audits),
				dispatch.WithNoJanitor())
			t.Cleanup(d.Close)
			// A queue nothing serves keeps a released run pending, so its status says whether the
			// approval took.
			held := &run.Run{
				ID: run.NewID(), Tool: run.ToolBash, Command: "deploy", Queue: "served-by-nobody",
				Status: run.StatusPendingApproval, Actor: "deploy-bot", ActorType: actorTypeAgent,
				CreatedAt: time.Now(), HeldByPolicy: "hold everything",
			}
			if err := store.Save(ctx, held); err != nil {
				t.Fatalf("store.Save() error = %v", err)
			}
			handler := approveRunHandler(d, store, nil, zap.NewNop())
			req := httptest.NewRequest(http.MethodPost, "/v1/runs/"+held.ID+"/approve", nil)
			req.SetPathValue("id", held.ID)
			reqCtx := withRecorded(context.WithValue(req.Context(), actorKey{}, test.Actor),
				test.Recorded)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req.WithContext(reqCtx))
			if diff := cmp.Diff(test.WantCode, rec.Code); diff != "" {
				t.Errorf("status mismatch (-want +got):\n%s\nbody: %s", diff, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), test.WantBody) {
				t.Errorf("body = %s, want it to carry %q", rec.Body.String(), test.WantBody)
			}
			got, err := store.Get(ctx, held.ID)
			if err != nil {
				t.Fatalf("store.Get() error = %v", err)
			}
			if diff := cmp.Diff(test.WantStatus, got.Status); diff != "" {
				t.Errorf("run status mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
