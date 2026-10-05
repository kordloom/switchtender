package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

// testInitiatorAndReasonRuleRoundTrip verifies the two fields a decision on a run reads survive the
// store: an agent-initiated run's identity evidence, and the reason requirement copied from the
// rules that held it. A run no agent asked for, under rules asking for no reason, reads back with
// neither.
//
// A store that dropped the initiator would present an agent's run as a person's in its committed
// outcome, and one that dropped the requirement would let a decision go through without the reason
// the rule demanded.
func testInitiatorAndReasonRuleRoundTrip(t *testing.T, store run.Store) {
	ctx := context.Background()
	tests := []struct {
		// ID is the run.
		ID string
		// WantInitiator is the agent identity saved and expected back.
		WantInitiator *run.Initiator
		// WantRequireReason is the requirement saved and expected back.
		WantRequireReason string
	}{{ // Test 0: An agent-initiated run held by a rule demanding reasons keeps both.
		ID: "run_agent_reason",
		WantInitiator: &run.Initiator{InitiatedBy: "deploy-bot", CredentialID: "tok_ab12",
			BoundTo: "dev-lead", ProvisionedBy: "org-admin", ProvisionedByType: "session"},
		WantRequireReason: "always",
	}, { // Test 1: A denial-only requirement round trips on a person's run.
		ID: "run_person_denials", WantRequireReason: "denials",
	}, { // Test 2: An ordinary run carries neither.
		ID: "run_plain",
	}}
	for _, test := range tests {
		if err := store.Save(ctx, &run.Run{
			ID: test.ID, Playbook: "site.yml", Status: run.StatusPendingApproval, CreatedAt: time.Now(),
			Initiator: test.WantInitiator, RequireReason: test.WantRequireReason,
		}); err != nil {
			t.Fatalf("Save(%s) error = %v", test.ID, err)
		}
		got, err := store.Get(ctx, test.ID)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", test.ID, err)
		}
		if diff := cmp.Diff(test.WantInitiator, got.Initiator); diff != "" {
			t.Errorf("%s Initiator mismatch (-want +got):\n%s", test.ID, diff)
		}
		if diff := cmp.Diff(test.WantRequireReason, got.RequireReason); diff != "" {
			t.Errorf("%s RequireReason mismatch (-want +got):\n%s", test.ID, diff)
		}
	}
}
