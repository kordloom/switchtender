package run

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestCloneIsolatesTheInitiator pins that a clone's agent identity is its own. The initiator is
// evidence committed with the run's outcome, so a reader holding a clone must not be able to
// rewrite who a stored run says asked for it, or who provisioned that agent.
func TestCloneIsolatesTheInitiator(t *testing.T) {
	t.Parallel()
	orig := &Run{ID: "run_agent", Initiator: &Initiator{InitiatedBy: "deploy-bot",
		CredentialID: "tok_1", BoundTo: "dev-lead", ProvisionedBy: "org-admin",
		ProvisionedByType: "session"}}
	clone := orig.Clone()
	clone.Initiator.BoundTo = "someone-else"
	clone.Initiator.ProvisionedBy = "someone-else"
	want := &Initiator{InitiatedBy: "deploy-bot", CredentialID: "tok_1", BoundTo: "dev-lead",
		ProvisionedBy: "org-admin", ProvisionedByType: "session"}
	if diff := cmp.Diff(want, orig.Initiator); diff != "" {
		t.Errorf("writing through the clone changed the original (-want +got):\n%s", diff)
	}
}

// TestInitiatorTravelsOnTheContext pins the context carrier the gate stamps and the dispatcher
// reads: what goes on comes back off as a copy, nothing comes off a context that carries none, and
// a nil initiator leaves the context as it was.
func TestInitiatorTravelsOnTheContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Put is what the gate places on the context, nil for nothing.
		Put *Initiator
		// WantInitiator is what the dispatcher reads back.
		WantInitiator *Initiator
	}{{ // Test 0: An agent's identity round trips.
		Put: &Initiator{InitiatedBy: "deploy-bot", BoundTo: "dev-lead",
			ProvisionedBy: "org-admin"},
		WantInitiator: &Initiator{InitiatedBy: "deploy-bot", BoundTo: "dev-lead",
			ProvisionedBy: "org-admin"},
	}, { // Test 1: A request that is not an agent's carries none.
		Put: nil, WantInitiator: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := WithInitiatorContext(context.Background(), test.Put)
			got := InitiatorFrom(ctx)
			if diff := cmp.Diff(test.WantInitiator, got); diff != "" {
				t.Errorf("initiator mismatch (-want +got):\n%s", diff)
			}
			if got != nil {
				got.BoundTo = "changed"
				if again := InitiatorFrom(ctx); again.BoundTo == "changed" {
					t.Error("a reader of the context changed what the next reader sees")
				}
			}
			r := &Run{}
			WithInitiator(test.Put)(r)
			if diff := cmp.Diff(test.WantInitiator, r.Initiator); diff != "" {
				t.Errorf("WithInitiator mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
