package relay

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// unruledPlan stands up a control node with no policy store, holding a live terraform plan a
// worker has claimed on behalf of who, and returns a client and the store.
func unruledPlan(t *testing.T, who *run.Run) (*Client, run.Store) {
	t.Helper()
	ctx := context.Background()
	store := run.NewMemStore()
	plan := &run.Run{
		ID: "run_plan", Status: run.StatusPending, CreatedAt: time.Now(),
		Queue: "default", Tool: run.ToolTerraform, Command: "infra/prod", OrgID: "org_1",
		Actor: who.Actor, ActorType: who.ActorType, Initiator: who.Initiator,
	}
	if err := store.Save(ctx, plan); err != nil {
		t.Fatalf("Save run: %v", err)
	}
	srv := httptest.NewServer(NewHandler(store, SinglePool("swt_worker"), zap.NewNop(), nil, nil,
		WithPlanSealer(testPlanSealer{})))
	t.Cleanup(srv.Close)
	tr := NewHTTPTransport(srv.URL, "swt_worker", nil)
	if _, err := tr.Claim(ctx, "worker-1", []string{"default"}); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return NewClient(tr), store
}

// TestAnAgentsApplyOnARelayWorkerIsHeldWithNoRules covers the deployment shape where a worker plans
// and the control node builds the apply. An install with no policy store still has the built-in
// agent hold, so an agent's plan proposes an apply that waits for a person and carries the agent's
// identity, while a person's apply on the same install has no gate that asked for a proposal.
func TestAnAgentsApplyOnARelayWorkerIsHeldWithNoRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Who is the actor the plan was submitted by.
		Who *run.Run
		// WantRefusal is the text a refused proposal carries, empty when one is created.
		WantRefusal string
	}{{ // Test 0: An agent's plan proposes an apply held by the built-in hold.
		Who: &run.Run{Actor: "release-agent", ActorType: policy.ActorKindAgent,
			Initiator: &run.Initiator{InitiatedBy: "release-agent", BoundTo: "dev-lead"}},
	}, { // Test 1: A person's plan asked for no proposal, the control.
		Who:         &run.Run{Actor: "dev-lead", ActorType: "session"},
		WantRefusal: "holds no approval policies",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			client, store := unruledPlan(t, test.Who)
			proposal, err := client.ProposeApply(context.Background(), "run_plan", 0, true,
				testPlanFile)
			if test.WantRefusal != "" {
				if err == nil || !strings.Contains(err.Error(), test.WantRefusal) {
					t.Fatalf("ProposeApply() error = %v, want a refusal saying %q", err,
						test.WantRefusal)
				}
				if got := applyOf(t, store); got != nil {
					t.Errorf("an apply was created for a plan no gate asked for: %+v", got.Status)
				}
				return
			}
			if err != nil {
				t.Fatalf("ProposeApply: %v", err)
			}
			if proposal.Status != run.StatusPendingApproval ||
				proposal.HeldByPolicy != policy.AgentDefaultName {
				t.Errorf("proposed apply is %q held by %q, want it held by %q", proposal.Status,
					proposal.HeldByPolicy, policy.AgentDefaultName)
			}
			stored := applyOf(t, store)
			if stored == nil || stored.Status != run.StatusPendingApproval {
				t.Fatalf("the stored apply does not match what was returned: %+v", stored)
			}
			if stored.Initiator == nil || stored.Initiator.InitiatedBy != "release-agent" {
				t.Errorf("stored apply initiator = %+v, want the agent that asked",
					stored.Initiator)
			}
		})
	}
}
