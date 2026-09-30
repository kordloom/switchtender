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

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// decisionEntries returns the request entry the gate recorded for a decision on runID and the
// DECISION entry the dispatcher committed for it, failing the test when either is missing.
func decisionEntries(t *testing.T, audits audit.Store, runID, decision, verdict string) (request,
	committed *audit.Entry) {
	t.Helper()
	chain, err := audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	for _, e := range chain {
		switch {
		case e.Method == http.MethodPost && e.Path == "/v1/runs/"+runID+"/"+decision:
			request = e
		case e.Method == audit.MethodDecision && e.Path == "/runs/"+runID+"/decision/"+verdict:
			committed = e
		}
	}
	if request == nil || committed == nil {
		t.Fatalf("the chain holds request entry %v and decision entry %v, want both", request != nil,
			committed != nil)
	}
	return request, committed
}

// decisionServer returns a server over a real dispatcher, a real gate, and a real chain, holding
// one run for approval, so a decision travels the whole path production takes: the gate records the
// request, the handler passes the decider on, and the dispatcher commits the decision.
func decisionServer(t *testing.T, tokens auth.Store, users user.Store) (http.Handler, audit.Store) {
	t.Helper()
	ctx := context.Background()
	store := run.NewMemStore()
	audits := audit.NewMemStore()
	held := &run.Run{
		ID: "run_held", Playbook: "site.yml", Inventory: "prod", Status: run.StatusPendingApproval,
		Actor: "casey", ActorType: actorTypeSession, CreatedAt: time.Now(),
	}
	if err := store.Save(ctx, held); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		},
	)
	d := dispatch.New(store, runner, zap.NewNop(), dispatch.WithAudits(audits),
		dispatch.WithNoJanitor())
	t.Cleanup(d.Close)
	handler := New(store, d, zap.NewNop(), WithTokens(tokens), WithUsers(users), WithAudit(audits),
		WithApprover(d)).Handler()
	return handler, audits
}

// TestADecisionCarriesTheAccountItsRequestCarries pins the DECISION entry to the identity of the
// request that made it.
//
// The gate records a decision's request with the token's label, how the caller authenticated, and
// the account the token is bound to. The DECISION entry carried only the first two, so the account
// behind a decision was recoverable only by pairing the entry with its request, and two tokens
// sharing a label on different accounts committed identical decisions. On an install serving open
// on loopback the request is recorded as "unauthenticated" and the decision named nobody, so the
// two entries for one decision disagreed about who made it.
//
//nolint:funlen // Test function.
func TestADecisionCarriesTheAccountItsRequestCarries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// Decision is the route taken, approve or reject.
		Decision string
		// Verdict is what the DECISION entry records.
		Verdict string
		// Bound serves an install with a token bound to an account, false serves one open.
		Bound bool
		// WantActor, WantType, and WantOnBehalfOf are what both entries must carry.
		WantActor      string
		WantType       string
		WantOnBehalfOf string
	}{{ // Test 0: A token bound to an account approves, and the decision names the account.
		Name: "token approves", Decision: "approve", Verdict: "approved", Bound: true,
		WantActor: "laptop", WantType: actorTypeToken, WantOnBehalfOf: "alice",
	}, { // Test 1: The same token rejects, and the rejection names it too.
		Name: "token rejects", Decision: "reject", Verdict: "rejected", Bound: true,
		WantActor: "laptop", WantType: actorTypeToken, WantOnBehalfOf: "alice",
	}, { // Test 2: An open install records the decision under the name its request has.
		Name: "open install approves", Decision: "approve", Verdict: "approved",
		WantActor: "unauthenticated", WantType: actorTypeUnauthenticated,
	}, { // Test 3: And the rejection.
		Name: "open install rejects", Decision: "reject", Verdict: "rejected",
		WantActor: "unauthenticated", WantType: actorTypeUnauthenticated,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			tokens, users := auth.NewMemStore(), user.NewMemStore()
			var plain string
			if test.Bound {
				alice, err := user.New("alice", "a long enough password", user.RoleAdmin)
				if err != nil {
					t.Fatalf("user.New() error = %v", err)
				}
				if err := users.Save(ctx, alice); err != nil {
					t.Fatalf("users.Save() error = %v", err)
				}
				var tok *auth.Token
				plain, tok, err = auth.New("laptop")
				if err != nil {
					t.Fatalf("auth.New() error = %v", err)
				}
				tok.UserID = alice.ID
				if err := tokens.Save(ctx, tok); err != nil {
					t.Fatalf("tokens.Save() error = %v", err)
				}
			}
			handler, audits := decisionServer(t, tokens, users)

			req := httptest.NewRequest(http.MethodPost, "/v1/runs/run_held/"+test.Decision,
				strings.NewReader(""))
			req.Host = "127.0.0.1:8080"
			if plain != "" {
				req.Header.Set("Authorization", "Bearer "+plain)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200 (%s)", test.Decision, rec.Code, rec.Body.String())
			}

			request, committed := decisionEntries(t, audits, "run_held", test.Decision, test.Verdict)
			for _, e := range []*audit.Entry{request, committed} {
				if e.Actor != test.WantActor || e.ActorType != test.WantType ||
					e.OnBehalfOf != test.WantOnBehalfOf {
					t.Errorf("%s %s entry = %q (%q) on behalf of %q, want %q (%q) on behalf of %q",
						e.Method, e.Path, e.Actor, e.ActorType, e.OnBehalfOf, test.WantActor,
						test.WantType, test.WantOnBehalfOf)
				}
			}
			// The account is committed by the link, not only stored beside it.
			if audit.EntryHash(committed) != committed.Hash {
				t.Error("the decision entry does not hash to its stored link")
			}
		})
	}
}
