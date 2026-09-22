package dispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestARetryCarriesTheActorTheGateJudges covers a gate that was reached and then handed nothing to
// judge.
//
// Making the retry face the approval policy closed the hole where retrying ran a spec an approver
// would have held. It left a narrower one open. The retry was built field by field from the parent
// and the retry request, and no actor was put on it, so every actor-scoped rule saw a run with no
// actor and declined to match. A policy written to hold one named agent's runs held the agent's
// original run and then let the retry of that same run through, which is the one submission path an
// operator reaches by clicking a button on the run the rule just stopped.
//
// Both scopings are covered, because they fail the same way for different reasons: the kind is read
// from the authentication type and the name from the actor, and neither was being set.
func TestARetryCarriesTheActorTheGateJudges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name   string
		Policy *policy.Policy
		Opts   []run.SubmitOption
	}{{ // Test 0: A rule scoped to one named actor.
		Name: "named actor",
		Policy: &policy.Policy{
			ID: policy.NewID(), Name: "hold that bot", Tool: "ansible",
			Actor: "release-bot", Effect: policy.EffectDeny,
		},
		Opts: []run.SubmitOption{run.WithActor("release-bot"), run.WithActorType("agent")},
	}, { // Test 1: A rule scoped to agents as a class.
		Name: "agent kind",
		Policy: &policy.Policy{
			ID: policy.NewID(), Name: "hold agents", Tool: "ansible",
			ActorKind: policy.ActorKindAgent, Effect: policy.EffectDeny,
		},
		Opts: []run.SubmitOption{run.WithActor("some-bot"), run.WithActorType("agent")},
	}}

	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			store := run.NewMemStore()
			policies := policy.NewMemStore()
			d := New(store, &failingLister{hosts: []string{"web01", "web02"}}, nil,
				WithPolicies(policies))
			defer d.Close()
			ctx := context.Background()

			// The original runs and its shards fail, which is what makes a retry available.
			parent, err := d.SubmitSplit(ctx, "site.yml", "inv", 2)
			if err != nil {
				t.Fatalf("test %d: SubmitSplit() error = %v", testNum, err)
			}
			waitForStatus(t, store, parent.ID, run.StatusFailed)

			if err := policies.Save(ctx, test.Policy); err != nil {
				t.Fatalf("test %d: policies.Save() error = %v", testNum, err)
			}

			// The same actor the rule names now clicks retry.
			_, err = d.RetryFailedShards(ctx, parent.ID, test.Opts...)
			if !errors.Is(err, ErrPolicyDenied) {
				t.Fatalf("test %d: RetryFailedShards() error = %v, want %v.\nThe rule that holds "+
					"this actor's runs did not match the retry, so clicking retry on a run the "+
					"policy stopped is a way around the policy", testNum, err, ErrPolicyDenied)
			}
		})
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

	// An agent submits and the run fails.
	parent, err := d.SubmitSplit(ctx, "site.yml", "inv", 2, run.WithActor("release-bot"),
		run.WithActorType("agent"))
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	waitForStatus(t, store, parent.ID, run.StatusFailed)

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
