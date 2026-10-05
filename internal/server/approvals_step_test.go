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

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// stepHarness is a server over a real dispatcher with an admin account, a person's token on it, and
// an agent token bound to the same account.
type stepHarness struct {
	// handler serves the API.
	handler http.Handler
	// store holds the runs.
	store run.Store
	// admin is the person's token, on an admin account.
	admin string
	// agent is an agent token bound to the same admin account.
	agent string
	// adminID is the admin account's id.
	adminID string
	// d is the dispatcher behind the API, for a test that calls it directly.
	d *dispatch.Dispatcher
}

// newStepHarness builds the harness.
func newStepHarness(t *testing.T) *stepHarness {
	t.Helper()
	ctx := context.Background()
	tokens, users := auth.NewMemStore(), user.NewMemStore()
	owner, err := user.New("owner", "a long enough password", user.RoleAdmin)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	if err := users.Save(ctx, owner); err != nil {
		t.Fatalf("users.Save() error = %v", err)
	}
	mint := func(label string, agent bool) string {
		plain, tok, merr := auth.New(label)
		if merr != nil {
			t.Fatalf("auth.New() error = %v", merr)
		}
		tok.UserID = owner.ID
		if agent {
			tok.Kind = auth.KindAgent
		}
		if serr := tokens.Save(ctx, tok); serr != nil {
			t.Fatalf("tokens.Save() error = %v", serr)
		}
		return plain
	}
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
	handler := New(store, d, zap.NewNop(), WithTokens(tokens), WithUsers(users), WithAudit(audits),
		WithApprover(d)).Handler()
	return &stepHarness{handler: handler, store: store, admin: mint("laptop", false),
		agent: mint("deploy-bot", true), adminID: owner.ID, d: d}
}

// call makes one authenticated request and returns the status and body.
func (h *stepHarness) call(t *testing.T, token, method, path, body string) (int, string) {
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
	h.handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// gatedPipeline is the body of a pipeline with one approval step between a build and a deploy.
const gatedPipeline = `{"name":"release","steps":[
	{"name":"build","tool":"bash","command":"make"},
	{"name":"gate","type":"approval","description":"Ship it?","depends_on":["build"]},
	{"name":"deploy","tool":"bash","command":"ship","depends_on":["gate"]}]}`

// submitGated submits the gated pipeline as token and waits until it waits at its step, returning
// the workflow's id and the waiting step as GET /approvals describes it.
func (h *stepHarness) submitGated(t *testing.T, token string) (string, approvalStepView) {
	t.Helper()
	code, body := h.call(t, token, http.MethodPost, "/v1/pipelines", gatedPipeline)
	if code != http.StatusAccepted {
		t.Fatalf("POST /v1/pipelines = %d %s, want 202", code, body)
	}
	var created run.Run
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode pipeline: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		code, body = h.call(t, token, http.MethodGet, "/v1/approvals", "")
		if code != http.StatusOK {
			t.Fatalf("GET /v1/approvals = %d %s, want 200", code, body)
		}
		var list approvalsResponse
		if err := json.Unmarshal([]byte(body), &list); err != nil {
			t.Fatalf("decode approvals: %v", err)
		}
		for _, a := range list.Approvals {
			if a.RunID == created.ID {
				return created.ID, a
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("workflow %s never appeared in the approvals queue", created.ID)
	return "", approvalStepView{}
}

// waitStatus waits for a run to reach want.
func (h *stepHarness) waitStatus(t *testing.T, id string, want run.Status) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, err := h.store.Get(context.Background(), id)
		if err == nil && r.Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s never reached %s", id, want)
}

// TestApprovalStepThroughTheAPI pins the approval queue and the decision routes for a workflow
// approval step: an agent sees the step it is waiting on and cannot approve it, the queue names
// what comes next, a decision bound to a stale state is refused, and an approver releases the
// workflow by the step's id. A decision posted to the workflow is refused, naming the step and the
// call that decides it, since only the step's own decision may move the workflow.
//
//nolint:funlen // Test function.
func TestApprovalStepThroughTheAPI(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Caller is admin or agent.
		Caller string
		// Target is step, workflow, or deny.
		Target string
		// Body is the request body, or shown for the digest the queue showed.
		Body string
		// WantCode is the response status.
		WantCode int
		// WantStatus is the workflow's status afterward.
		WantStatus run.Status
	}{{ // Test 0: The agent that launched the workflow cannot approve its step.
		Caller: "agent", Target: "step", WantCode: http.StatusForbidden,
		WantStatus: run.StatusPendingApproval,
	}, { // Test 1: A decision bound to a state the approver was not shown is refused.
		Caller: "admin", Target: "step", Body: `{"state_digest":"sha256:stale"}`,
		WantCode: http.StatusConflict, WantStatus: run.StatusPendingApproval,
	}, { // Test 2: An approver approves the step by its id, bound to what the queue showed.
		Caller: "admin", Target: "step", Body: "shown", WantCode: http.StatusOK,
		WantStatus: run.StatusSucceeded,
	}, { // Test 3: An approval posted to the workflow is refused and the workflow keeps waiting.
		Caller: "admin", Target: "workflow", WantCode: http.StatusConflict,
		WantStatus: run.StatusPendingApproval,
	}, { // Test 4: A denial posted to the workflow is refused and the workflow keeps waiting.
		Caller: "admin", Target: "deny", WantCode: http.StatusConflict,
		WantStatus: run.StatusPendingApproval,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			h := newStepHarness(t)
			workflow, step := h.submitGated(t, h.agent)
			if step.Step != "gate" || step.Description != "Ship it?" ||
				strings.Join(step.OnApprove, ",") != "deploy" || len(step.Upstream) != 1 ||
				step.Upstream[0].Status != string(run.StatusSucceeded) || step.StateDigest == "" ||
				step.RequestedByType != actorTypeAgent {
				t.Fatalf("queue entry = %+v, want the gate after build, before deploy", step)
			}
			token := h.admin
			if test.Caller == "agent" {
				token = h.agent
			}
			path := "/v1/runs/" + step.ID + "/approve"
			switch test.Target {
			case "workflow":
				path = "/v1/runs/" + workflow + "/approve"
			case "deny":
				path = "/v1/runs/" + workflow + "/reject"
			}
			body := test.Body
			if body == "shown" {
				body = `{"state_digest":"` + step.StateDigest + `"}`
			}
			code, resp := h.call(t, token, http.MethodPost, path, body)
			if code != test.WantCode {
				t.Fatalf("POST %s = %d %s, want %d", path, code, resp, test.WantCode)
			}
			decides := "POST /v1/runs/" + step.ID + "/approve"
			if test.Target != "step" && !strings.Contains(resp, decides) {
				t.Errorf("the refusal %s does not name %s, the call that decides it", resp, decides)
			}
			h.waitStatus(t, workflow, test.WantStatus)
		})
	}
}

// TestApprovalStepSelfApprovalUsesTheAccount pins separation of duties on a step to the account,
// the way a held run's is: the person who launched the workflow cannot approve its step from
// another credential when the rule in force requires a different approver, and may when it does
// not.
func TestApprovalStepSelfApprovalUsesTheAccount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Distinct requires a different approver than the launcher.
		Distinct bool
		// WantCode is the response status.
		WantCode int
	}{{ // Test 0: A rule requiring a second person refuses the launcher's own approval.
		Distinct: true, WantCode: http.StatusConflict,
	}, { // Test 1: Without that rule the launcher may approve, as a held run allows.
		Distinct: false, WantCode: http.StatusOK,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			h := newStepHarness(t)
			parentID := run.NewID()
			parent := &run.Run{ID: parentID, Playbook: "release", Kind: run.KindPipeline,
				Status: run.StatusPendingApproval, CreatedAt: time.Now(), Actor: "cli-token",
				ActorUserID: h.adminID, Steps: []run.PipelineStep{{Name: "gate", Type: run.StepApproval}}}
			if err := h.store.Save(ctx, parent); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			idx := 0
			node := &run.Run{ID: run.NewID(), Kind: run.KindApproval, ParentID: &parentID,
				StepIndex: &idx, StepName: "gate", Status: run.StatusPendingApproval,
				CreatedAt: time.Now(), Actor: "cli-token", ActorUserID: h.adminID,
				RequireDistinctApprover: test.Distinct}
			if err := h.store.Save(ctx, node); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			code, body := h.call(t, h.admin, http.MethodPost, "/v1/runs/"+node.ID+"/approve", "")
			if code != test.WantCode {
				t.Fatalf("approve = %d %s, want %d", code, body, test.WantCode)
			}
		})
	}
}

// TestStateDigestIsRefusedOnARun pins that a state digest sent with a whole run's decision is
// refused rather than ignored, since a binding the caller asked for and did not get is worse than
// an error.
func TestStateDigestIsRefusedOnARun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newStepHarness(t)
	held := &run.Run{ID: run.NewID(), Playbook: "site.yml", Status: run.StatusPendingApproval,
		CreatedAt: time.Now()}
	if err := h.store.Save(ctx, held); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	code, body := h.call(t, h.admin, http.MethodPost, "/v1/runs/"+held.ID+"/approve",
		`{"state_digest":"sha256:x"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("approve with a state digest = %d %s, want 400", code, body)
	}
}
