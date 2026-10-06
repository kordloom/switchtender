package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// agentLabeledOn mints an agent token labeled label, bound to a new operator account named owner,
// and returns the token. Labels are chosen by whoever mints a token, so two accounts can each hold
// an agent token under the same one.
func agentLabeledOn(t *testing.T, s *agentHoldServer, label, owner string) string {
	t.Helper()
	ctx := context.Background()
	u, err := user.New(owner, "a long enough password", user.RoleOperator)
	if err != nil {
		t.Fatalf("user.New(%s) error = %v", owner, err)
	}
	if err := s.DB.users.Save(ctx, u); err != nil {
		t.Fatalf("users.Save(%s) error = %v", owner, err)
	}
	plain, tok, err := auth.New(label)
	if err != nil {
		t.Fatalf("auth.New(%s) error = %v", label, err)
	}
	tok.UserID, tok.Kind = u.ID, auth.KindAgent
	if err := s.DB.tokens.Save(ctx, tok); err != nil {
		t.Fatalf("tokens.Save(%s) error = %v", label, err)
	}
	return plain
}

// TestAnExemptionCoversOneAccountsAgent is the label collision through the API, on every store.
// Two agent tokens are labeled release-agent, one bound to dev-lead and one to ops-lead, and the
// exemption names the label on dev-lead. The dev-lead agent's run goes ahead, and so do the runs
// it causes indirectly: a rerun, a failed-host relaunch, a retry of a split with its shards, a
// template launch, and a workflow with its steps. The ops-lead agent's identical requests are each
// held by default, on an install of their own, since a rerun inside the dedupe window returns the
// rerun already made. Every run records the account it was asked for under.
//
//nolint:funlen // Test function.
func TestAnExemptionCoversOneAccountsAgent(t *testing.T) {
	t.Parallel()
	const name = "release agent on dev-lead"
	exempt := `{"name":"` + name + `","effect":"exempt","actor":"release-agent",` +
		`"account":"dev-lead"}`
	tests := []struct {
		// Path and Body are the request.
		Path string
		// Body is the request body.
		Body string
	}{{ // Test 0: A run submitted directly.
		Path: "/v1/runs", Body: `{"tool":"bash","command":"./deploy.sh"}`,
	}, { // Test 1: A rerun of a person's finished run.
		Path: "/v1/runs/run_done/rerun", Body: `{}`,
	}, { // Test 2: A relaunch of a person's failed hosts.
		Path: "/v1/runs/run_broken/relaunch-failed", Body: `{}`,
	}, { // Test 3: A retry of a person's failed split, whose shards come with it.
		Path: "/v1/runs/run_split/retry", Body: `{}`,
	}, { // Test 4: A template launch.
		Path: "/v1/templates/tpl_deploy/launch", Body: `{}`,
	}, { // Test 5: A workflow submitted whole, whose steps are judged with it.
		Path: "/v1/pipelines",
		Body: `{"name":"release","steps":[{"name":"build","tool":"bash","command":"./build.sh"},` +
			`{"name":"ship","tool":"bash","command":"./ship.sh"}]}`,
	}}
	sides := []struct {
		// Owner is the account the requesting agent's token is bound to.
		Owner string
		// WantHeld is whether its request waits for a person.
		WantHeld bool
		// WantNote is the note its run carries.
		WantNote string
	}{
		{Owner: "dev-lead", WantNote: exemptNote(name)},
		{Owner: "ops-lead", WantHeld: true, WantNote: policy.AgentDefaultName},
	}
	for _, backend := range agentHoldBackends() {
		for testNum, test := range tests {
			for _, side := range sides {
				t.Run(fmt.Sprintf("%s test %d %s", backend.Name, testNum, side.Owner),
					func(t *testing.T) {
						t.Parallel()
						s := newAgentHoldServer(t, backend.Open(t), true)
						agentHoldSeed(t, s.DB)
						// The ops-lead agent has the same label as the dev-lead agent the
						// harness minted.
						token := s.Agent
						if side.Owner != "dev-lead" {
							token = agentLabeledOn(t, s, "release-agent", side.Owner)
						}
						if code, got := s.call(s.Admin, http.MethodPost, "/v1/policies",
							exempt); code != http.StatusCreated {
							t.Fatalf("admin writing the exemption = %d: %s", code, got)
						}
						got := s.submit(t, token, http.MethodPost, test.Path, test.Body)
						if held := got.Status == run.StatusPendingApproval; held != side.WantHeld {
							t.Errorf("held = %v, want %v (held by %q)", held, side.WantHeld,
								got.HeldByPolicy)
						}
						if got.Actor != "release-agent" || got.Account != side.Owner {
							t.Errorf("run names %s on %q, want release-agent on %q", got.Actor,
								got.Account, side.Owner)
						}
						if !slices.Contains(got.PolicyNotes, side.WantNote) {
							t.Errorf("notes = %q, want %q", got.PolicyNotes, side.WantNote)
						}
						children, err := s.DB.runs.Shards(context.Background(), got.ID)
						if err != nil {
							t.Fatalf("Shards() error = %v", err)
						}
						for _, child := range children {
							if child.Account != side.Owner {
								t.Errorf("child %s account = %q, want %q", child.ID,
									child.Account, side.Owner)
							}
						}
					})
			}
		}
	}
}

// TestTheExemptionAccountThroughTheAPI holds the API to the account rule and shows it in the policy
// list. An exemption naming an agent's label without its account is refused with a reason before
// anything is stored, one naming both is stored and listed with its account, and a rule that holds
// may still name a label alone, since a hold that matches too much fails safe.
func TestTheExemptionAccountThroughTheAPI(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Body is the policy written.
		Body string
		// WantCode is the answer.
		WantCode int
		// WantBody is text the answer must hold.
		WantBody string
	}{{ // Test 0: The label alone is refused, saying why.
		Body:     `{"name":"ci","effect":"exempt","actor":"release-agent"}`,
		WantCode: http.StatusBadRequest, WantBody: "not unique across accounts",
	}, { // Test 1: The label with its account is stored with it.
		Body:     `{"name":"ci","effect":"exempt","actor":"release-agent","account":"dev-lead"}`,
		WantCode: http.StatusCreated, WantBody: `"account":"dev-lead"`,
	}, { // Test 2: The account alone is stored.
		Body:     `{"name":"lead","effect":"exempt","account":"dev-lead"}`,
		WantCode: http.StatusCreated, WantBody: `"account":"dev-lead"`,
	}, { // Test 3: A hold naming a label alone is accepted, the control for test 0.
		Body:     `{"name":"bot waits","actor":"release-agent"}`,
		WantCode: http.StatusCreated, WantBody: `"actor":"release-agent"`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			s := newAgentHoldServer(t, agentHoldBackends()[0].Open(t), true)
			code, got := s.call(s.Admin, http.MethodPost, "/v1/policies", test.Body)
			if code != test.WantCode || !strings.Contains(got, test.WantBody) {
				t.Fatalf("POST /v1/policies = %d %s, want %d holding %s", code, got,
					test.WantCode, test.WantBody)
			}
			code, listed := s.call(s.Admin, http.MethodGet, "/v1/policies", "")
			stored := test.WantCode == http.StatusCreated
			if code != http.StatusOK || strings.Contains(listed, test.WantBody) != stored {
				t.Errorf("GET /v1/policies = %d %s, want the rule listed = %v", code, listed,
					stored)
			}
		})
	}
}
