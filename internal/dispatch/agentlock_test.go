package dispatch

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// TestApproveRefusesAnAgentDeciderOnItsOwn proves the dispatcher's lock on a held run stands alone.
// The door caps an agent's token below the role the approve route needs, so through the API this
// lock is the second one. Here there is no door at all: the dispatcher is called directly with an
// agent decider, which is the shape of any future path that reaches it without going through the
// gate. Nothing may be recorded and nothing may be released.
//
// The person cases are the negative control. The same call with a person deciding releases the run,
// so the refusal below is the agent check and not something else about the run.
func TestApproveRefusesAnAgentDeciderOnItsOwn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// By is who decides.
		By outcome.Decider
		// WantStatus is the run's status after the call.
		WantStatus run.Status
		// WantDecisions is how many decision entries the chain holds afterward.
		WantDecisions int
		// Want is the error the call returns.
		Want error
	}{{ // Test 0: An agent decider is refused, the run stays held, and the chain holds no decision.
		By:         outcome.Decider{Name: "deploy-bot", Type: "agent", OnBehalfOf: "ops-admin"},
		WantStatus: run.StatusPendingApproval, WantDecisions: 0, Want: ErrAgentApproval,
	}, { // Test 1: An agent decider bound to nobody is refused the same way.
		By:         outcome.Decider{Name: "deploy-bot", Type: "agent"},
		WantStatus: run.StatusPendingApproval, WantDecisions: 0, Want: ErrAgentApproval,
	}, { // Test 2: A person's session releases the same run, so the lock is what refused the agent.
		By:         outcome.Decider{Name: "ops-admin", Type: "session", OnBehalfOf: "ops-admin"},
		WantStatus: run.StatusPending, WantDecisions: 1, Want: nil,
	}, { // Test 3: A person's own token releases it too.
		By:         outcome.Decider{Name: "laptop", Type: "token", OnBehalfOf: "ops-admin"},
		WantStatus: run.StatusPending, WantDecisions: 1, Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			audits := audit.NewMemStore()
			d := New(store, okRunner(), zap.NewNop(), WithAudits(audits), WithNoJanitor())
			t.Cleanup(d.Close)
			// The run is routed to a queue this dispatcher does not serve, so a released run stays
			// pending, which is what lets the status say whether the approval took.
			held := &run.Run{
				ID: run.NewID(), Tool: run.ToolBash, Command: "deploy", Status: run.StatusPendingApproval,
				Actor: "deploy-bot", ActorType: "agent", CreatedAt: d.now(),
				HeldByPolicy: "hold everything", Queue: "served-by-nobody",
			}
			if err := store.Save(ctx, held); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			_, err := d.Approve(ctx, held.ID, test.By)
			if !errors.Is(err, test.Want) {
				t.Fatalf("Approve() error = %v, want %v", err, test.Want)
			}
			got, err := store.Get(ctx, held.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if diff := cmp.Diff(test.WantStatus, got.Status); diff != "" {
				t.Errorf("status mismatch (-want +got):\n%s", diff)
			}
			chain, err := audits.Chain(ctx)
			if err != nil {
				t.Fatalf("Chain() error = %v", err)
			}
			decisions := 0
			for _, e := range chain {
				if e.Method == audit.MethodDecision {
					decisions++
				}
			}
			if diff := cmp.Diff(test.WantDecisions, decisions); diff != "" {
				t.Errorf("decision entries mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
