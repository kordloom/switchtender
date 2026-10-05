package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// testAccountRoundTrip verifies the account a run was requested under survives the store, read by
// id and in a list, and that a later save of the run keeps the value it carries. A policy's account
// criterion reads it, so a store that dropped it would turn every agent's run into one whose
// account is unknown: held by an exemption written for its account, and held by every rule scoped
// to an account, including accounts the run was never requested under.
func testAccountRoundTrip(t *testing.T, store run.Store) {
	ctx := context.Background()
	tests := []struct {
		// ID is the run.
		ID string
		// WantAccount is the account saved and expected back.
		WantAccount string
	}{{ // Test 0: An agent's run keeps the account its token is bound to.
		ID: "run_account_agent", WantAccount: "team-a",
	}, { // Test 1: A run no account stands behind keeps none.
		ID: "run_account_none",
	}}
	for _, test := range tests {
		r := &run.Run{ID: test.ID, Playbook: "site.yml", Status: run.StatusPendingApproval,
			CreatedAt: time.Now(), Actor: "ci-agent", ActorType: "agent",
			Account: test.WantAccount}
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", test.ID, err)
		}
		// A whole-row save of the same run, the way every status change writes it, keeps it.
		r.Status = run.StatusPending
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) again error = %v", test.ID, err)
		}
		got, err := store.Get(ctx, test.ID)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", test.ID, err)
		}
		if got.Account != test.WantAccount {
			t.Errorf("Get(%s) account = %q, want %q", test.ID, got.Account, test.WantAccount)
		}
	}
	listed, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, r := range listed {
		if r.ID == "run_account_agent" && r.Account != "team-a" {
			t.Errorf("List() account = %q, want team-a", r.Account)
		}
	}
}
