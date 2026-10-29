package dispatch

import (
	"context"
	"testing"

	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestARetryCarriesTheActorTheGateJudges covers a gate that was reached and then handed nothing to
// judge.
//
// Making the retry face the approval policy closed the hole where retrying ran a spec an approver
// would have held. It left a narrower one open. The retry was built field by field from the parent
// and the retry request, and no actor was put on it, so every actor-scoped rule saw a run with no
// actor and declined to match, and the built-in hold on an agent's run never saw an agent. The
// retry has to carry who asked for it, by name and by kind, since the kind is read from the
// authentication type and the name from the actor, and neither was being set.
func TestARetryCarriesTheActorTheGateJudges(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	d := New(store, &failingLister{hosts: []string{"web01", "web02"}}, nil,
		WithPolicies(policy.NewMemStore()))
	defer d.Close()
	ctx := context.Background()

	// The original runs and its shards fail, which is what makes a retry available.
	parent, err := d.SubmitSplit(ctx, "site.yml", "inv", 2)
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	dispatchtest.WaitForStatus(t, store, parent.ID, run.StatusFailed)

	retry, err := d.RetryFailedShards(ctx, parent.ID, run.WithActor("release-bot"),
		run.WithActorType(policy.ActorKindAgent))
	if err != nil {
		t.Fatalf("RetryFailedShards() error = %v", err)
	}
	if retry.Actor != "release-bot" || retry.ActorType != policy.ActorKindAgent {
		t.Errorf("retry actor = %q/%q, want release-bot/agent: a rule scoped to who asked never "+
			"sees the retry", retry.Actor, retry.ActorType)
	}
	if retry.Status != run.StatusPendingApproval {
		t.Errorf("an agent's retry is %q, want held by the built-in agent hold: the gate judged a "+
			"run with no actor on it", retry.Status)
	}
}

// TestARetryOfSomebodyElsesRunIsJudgedOnWhoRetried pins which actor the gate reads, since carrying
// the wrong one is as bad as carrying none.
//
// The retry is authorized by the retry request, not by whatever authorized the parent weeks ago, so
// the actor on it is whoever clicked retry. Inheriting the parent's actor instead would judge a
// person's retry against the rule written for the agent that first ran it, and would credit the run
// to somebody who did not ask for it.
func TestARetryOfSomebodyElsesRunIsJudgedOnWhoRetried(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	policies := policy.NewMemStore()
	d := New(store, &failingLister{hosts: []string{"web01", "web02"}}, nil, WithPolicies(policies))
	defer d.Close()
	ctx := context.Background()

	// The agent's own run goes ahead under an exemption, since an agent's run is otherwise held by
	// default and this one has to fail for a retry to exist.
	if err := policies.Save(ctx, &policy.Policy{
		ID: policy.NewID(), Name: "let the bot run", Actor: "release-bot", Account: "release-owner",
		Effect: policy.EffectExempt, MaxDestroy: policy.DisabledMaxDestroy,
	}); err != nil {
		t.Fatalf("policies.Save() error = %v", err)
	}

	// An agent submits and the run fails.
	parent, err := d.SubmitSplit(ctx, "site.yml", "inv", 2, run.WithActor("release-bot"),
		run.WithActorType("agent"), run.WithAccount("release-owner"))
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	dispatchtest.WaitForStatus(t, store, parent.ID, run.StatusFailed)

	if err := policies.Save(ctx, &policy.Policy{
		ID: policy.NewID(), Name: "hold that bot", Tool: "ansible",
		Actor: "release-bot", Effect: policy.EffectDeny,
	}); err != nil {
		t.Fatalf("policies.Save() error = %v", err)
	}

	// A person retries it. The rule names the agent, not them, so it does not apply.
	retry, err := d.RetryFailedShards(ctx, parent.ID, run.WithActor("casey"),
		run.WithActorType("session"))
	if err != nil {
		t.Fatalf("a person's retry was judged against the rule written for the agent that first "+
			"ran it: RetryFailedShards() error = %v", err)
	}
	if retry.Actor != "casey" {
		t.Errorf("the retry is credited to %q, want %q: asking what a given operator started "+
			"misses the runs they started this way", retry.Actor, "casey")
	}
}
