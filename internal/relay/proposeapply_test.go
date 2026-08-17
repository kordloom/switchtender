package relay

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// planFixture stands up a control node holding one live terraform plan a destroy policy scopes, with a
// worker that has claimed it. Each case builds its own, because a plan proposes exactly one apply: the
// call is idempotent so a worker whose response was lost cannot mint a second real change, which means
// two outcomes cannot be exercised against one plan.
func planFixture(t *testing.T) (client *Client, store run.Store, baseURL string) {
	t.Helper()
	ctx := context.Background()
	store = run.NewMemStore()
	policies := policy.NewMemStore()
	if err := policies.Save(ctx, &policy.Policy{
		ID: "pol_1", Name: "tf-destroy-guard", Tool: run.ToolTerraform, MaxDestroy: 1,
	}); err != nil {
		t.Fatalf("Save policy: %v", err)
	}
	now := time.Now()
	plan := &run.Run{
		ID: "run_plan", Status: run.StatusPending, CreatedAt: now,
		Queue: "default", Tool: run.ToolTerraform, Command: "infra/prod",
		DryRun: true, Actor: "casey", ActorType: "session", OrgID: "org_1",
		CommitSHA: "abc123", ProjectID: "prj_1",
	}
	if err := store.Save(ctx, plan); err != nil {
		t.Fatalf("Save run: %v", err)
	}

	srv := httptest.NewServer(NewHandler(store, SinglePool("ymt_worker"), zap.NewNop(), policies, nil))
	t.Cleanup(srv.Close)
	tr := NewHTTPTransport(srv.URL, "ymt_worker", nil)
	// The worker claims the plan the way it would in service, which is what issues the per-claim
	// capability every later call presents.
	if _, err := tr.Claim(ctx, "worker-1", []string{"default"}); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return NewClient(tr), store, srv.URL
}

// TestAWorkerCanProposeTheApplyItsPlanGated covers a gate that could not complete anywhere but the
// control node. A terraform run a plan-content policy scopes is planned first, and the apply it would
// perform is proposed as a second run for a person to release. A relay worker has no path to create a
// run: the save endpoint refuses an unknown id on purpose, since a worker only reports on what it
// claimed. So on a worker the proposal failed with a 404, the plan run failed with it, and the gated
// apply was never held for anybody. The most careful thing in the product did nothing in the topology
// it is most needed in.
//
// The worker now reports what its plan found, a destroy count and whether the summary was readable, and
// the control node builds the proposal itself from the plan run it already holds. That is narrower than
// letting a worker submit a run: the apply's command, target, credentials, and commit come from the
// stored plan rather than from the worker's request, so a worker cannot propose an apply of something
// else.
func TestAWorkerCanProposeTheApplyItsPlanGated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, store, _ := planFixture(t)

	// Test 0: A plan over the threshold produces a held apply, built from the plan the control node has.
	proposal, err := client.ProposeApply(ctx, "run_plan", 3, true)
	if err != nil {
		t.Fatalf("ProposeApply = %v, want a held apply", err)
	}
	if proposal.Status != run.StatusPendingApproval {
		t.Errorf("proposed apply status = %q, want pending_approval", proposal.Status)
	}
	if proposal.DryRun {
		t.Error("the proposed apply is a dry run, so it would preview rather than apply")
	}
	for _, tc := range []struct {
		Field, Got, Want string
	}{
		{"proposed_from", proposal.ProposedFrom, "run_plan"},
		{"command", proposal.Command, "infra/prod"},
		{"actor", proposal.Actor, "casey"},
		{"org", proposal.OrgID, "org_1"},
		{"pinned commit", proposal.PinnedCommit, "abc123"},
	} {
		if tc.Got != tc.Want {
			t.Errorf("proposed apply %s = %q, want %q", tc.Field, tc.Got, tc.Want)
		}
	}
	// It is really on the control node, not only in the response.
	if _, err := store.Get(ctx, proposal.ID); err != nil {
		t.Errorf("the proposal is not in the control node's store: %v", err)
	}

	// Test 1: A plan under the threshold proposes an apply that runs without waiting.
	under, _, _ := planFixture(t)
	queued, err := under.ProposeApply(ctx, "run_plan", 0, true)
	if err != nil {
		t.Fatalf("ProposeApply under the threshold = %v", err)
	}
	if queued.Status != run.StatusPending {
		t.Errorf("apply within the threshold = %q, want pending", queued.Status)
	}

	// Test 2: A plan whose summary could not be read is held, never queued, because a plan nobody could
	// weigh against the limit has not passed it.
	unreadable, _, _ := planFixture(t)
	unread, err := unreadable.ProposeApply(ctx, "run_plan", 0, false)
	if err != nil {
		t.Fatalf("ProposeApply of an unreadable plan = %v", err)
	}
	if unread.Status != run.StatusPendingApproval {
		t.Errorf("apply from an unreadable plan = %q, want pending_approval", unread.Status)
	}
	if unread.HeldByPolicy == "" {
		t.Error("the held apply does not say what held it")
	}

	// Test 3: A worker without the run's lease cannot propose anything for it.
	_, _, bareURL := planFixture(t)
	bare := NewClient(NewHTTPTransport(bareURL, "ymt_worker", nil))
	if _, err := bare.ProposeApply(ctx, "run_plan", 3, true); err == nil {
		t.Error("a worker with no lease proposed an apply for somebody else's run")
	}
}
