package server

import (
	"context"
	"encoding/json"
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
	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// agentOrg is an install where an organization admin provisions an agent bound to somebody else's
// account: org-admin mints the token, dev-lead is the account it acts under, and both are people
// who may approve. It is the shape that makes the issuer and the bound account differ.
type agentOrg struct {
	// handler serves the API.
	handler http.Handler
	// store holds the runs.
	store run.Store
	// decisions holds the decision records.
	decisions decision.Store
	// orgAdmin is org-admin's own token.
	orgAdmin string
	// devLead is dev-lead's own token.
	devLead string
	// agent is the agent token org-admin minted for dev-lead, through the API.
	agent string
}

// newAgentOrg builds the install, with one rule that holds every run and, when distinct is set,
// requires an independent approver.
func newAgentOrg(t *testing.T, distinct bool) *agentOrg {
	t.Helper()
	ctx := context.Background()
	tokens, users := auth.NewMemStore(), user.NewMemStore()
	mint := func(username string) string {
		u, err := user.New(username, "a long enough password", user.RoleAdmin)
		if err != nil {
			t.Fatalf("user.New() error = %v", err)
		}
		if err := users.Save(ctx, u); err != nil {
			t.Fatalf("users.Save() error = %v", err)
		}
		plain, tok, err := auth.New(username)
		if err != nil {
			t.Fatalf("auth.New() error = %v", err)
		}
		tok.UserID = u.ID
		if err := tokens.Save(ctx, tok); err != nil {
			t.Fatalf("tokens.Save() error = %v", err)
		}
		return plain
	}
	o := &agentOrg{orgAdmin: mint("org-admin"), devLead: mint("dev-lead")}
	policies := policy.NewMemStore()
	if err := policies.Save(ctx, &policy.Policy{ID: "pol_all", Name: "hold everything",
		MaxDestroy: policy.DisabledMaxDestroy, RequireDistinctApprover: distinct,
		CreatedAt: time.Now()}); err != nil {
		t.Fatalf("policies.Save() error = %v", err)
	}
	o.store = run.NewMemStore()
	o.decisions = decision.NewMemStore()
	audits := audit.NewMemStore()
	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		},
	)
	d := dispatch.New(o.store, runner, zap.NewNop(), dispatch.WithAudits(audits),
		dispatch.WithPolicies(policies), dispatch.WithDecisions(o.decisions), dispatch.WithNoJanitor())
	t.Cleanup(d.Close)
	o.handler = New(o.store, d, zap.NewNop(), WithTokens(tokens), WithUsers(users),
		WithAudit(audits), WithApprover(d), WithDecisions(o.decisions)).Handler()
	code, body := o.call(t, o.orgAdmin, http.MethodPost, "/v1/tokens",
		`{"name":"deploy-bot","username":"dev-lead","kind":"agent"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /v1/tokens = %d %s, want 201", code, body)
	}
	var minted createTokenResponse
	if err := json.Unmarshal([]byte(body), &minted); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if minted.CreatedBy != "org-admin" {
		t.Fatalf("minted token issuer = %q, want org-admin", minted.CreatedBy)
	}
	o.agent = minted.Token
	return o
}

// call makes one authenticated request and returns the status and body.
func (o *agentOrg) call(t *testing.T, token, method, path, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	o.handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// submit has the agent ask for a run and returns it as stored.
func (o *agentOrg) submit(t *testing.T) *run.Run {
	t.Helper()
	code, body := o.call(t, o.agent, http.MethodPost, "/v1/runs",
		`{"tool":"bash","command":"deploy","queue":"served-by-nobody"}`)
	if code != http.StatusAccepted {
		t.Fatalf("POST /v1/runs as the agent = %d %s, want 202", code, body)
	}
	var created run.Run
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	stored, err := o.store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	return stored
}

// TestAnAgentsRunRecordsWhoProvisionedIt pins the identity evidence an agent-initiated run carries:
// the agent, the account it is bound to, and who minted its token, observed from the token when the
// run was asked for. The issuer and the bound account differ here, which is the case that makes
// recording both necessary.
func TestAnAgentsRunRecordsWhoProvisionedIt(t *testing.T) {
	t.Parallel()
	o := newAgentOrg(t, false)
	held := o.submit(t)
	if held.Status != run.StatusPendingApproval {
		t.Fatalf("status = %s, want held", held.Status)
	}
	want := &run.Initiator{InitiatedBy: "deploy-bot", BoundTo: "dev-lead",
		ProvisionedBy: "org-admin", ProvisionedByType: "token"}
	got := held.Initiator
	if got != nil {
		got.CredentialID = ""
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("initiator mismatch (-want +got):\n%s", diff)
	}
	// A person's own run carries no agent identity.
	code, body := o.call(t, o.devLead, http.MethodPost, "/v1/runs",
		`{"tool":"bash","command":"deploy","queue":"served-by-nobody"}`)
	if code != http.StatusAccepted {
		t.Fatalf("POST /v1/runs as a person = %d %s", code, body)
	}
	var mine run.Run
	if err := json.Unmarshal([]byte(body), &mine); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if stored, _ := o.store.Get(context.Background(), mine.ID); stored.Initiator != nil {
		t.Errorf("a person's run carries an agent identity: %+v", stored.Initiator)
	}
}

// TestTheBoundAccountCountsAsTheRequester pins the rule for who may approve an agent's run. The
// account the agent is bound to may approve it unless a separation-of-duties rule requires an
// independent approver, and when one does, that account counts as the requester and is refused.
// Each decision that goes through records the evaluation.
func TestTheBoundAccountCountsAsTheRequester(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Distinct is whether the rule requires an independent approver.
		Distinct bool
		// Approver is who decides: dev-lead, the bound account, or org-admin.
		Approver string
		// WantCode is the decision's response.
		WantCode int
		// WantSoD is the evaluation recorded, nil when the decision is refused.
		WantSoD *decision.SeparationOfDuties
	}{{ // Test 0: No rule requires otherwise, so the bound account approves its agent's run.
		Distinct: false, Approver: "dev-lead", WantCode: http.StatusOK,
		WantSoD: &decision.SeparationOfDuties{Requester: "dev-lead", Decider: "dev-lead",
			Result: decision.SoDNotRequired},
	}, { // Test 1: With separation of duties on, the bound account is the requester and is refused.
		Distinct: true, Approver: "dev-lead", WantCode: http.StatusConflict,
	}, { // Test 2: An independent person approves, and the requirement is recorded as satisfied.
		Distinct: true, Approver: "org-admin", WantCode: http.StatusOK,
		WantSoD: &decision.SeparationOfDuties{Required: true, Requester: "dev-lead",
			Decider: "org-admin", Independent: true, Result: decision.SoDSatisfied},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			o := newAgentOrg(t, test.Distinct)
			held := o.submit(t)
			token := o.devLead
			if test.Approver == "org-admin" {
				token = o.orgAdmin
			}
			code, body := o.call(t, token, http.MethodPost, "/v1/runs/"+held.ID+"/approve", "")
			if code != test.WantCode {
				t.Fatalf("approve as %s = %d %s, want %d", test.Approver, code, body, test.WantCode)
			}
			records, err := o.decisions.ForRun(context.Background(), held.ID)
			if err != nil {
				t.Fatalf("ForRun() error = %v", err)
			}
			if test.WantSoD == nil {
				if len(records) != 0 {
					t.Errorf("a refused approval left %d decision records", len(records))
				}
				return
			}
			if len(records) != 1 {
				t.Fatalf("decision records = %d, want 1", len(records))
			}
			if diff := cmp.Diff(test.WantSoD, records[0].SeparationOfDuties); diff != "" {
				t.Errorf("separation of duties mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
