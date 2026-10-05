package dispatch

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// releaseOnDevLead is the exemption the account tests write: the agent labeled release-agent, on
// the dev-lead account and no other.
const releaseOnDevLead = "release agent on dev-lead"

// boundAgent returns the context and options a request from the agent token labeled release-agent
// carries when the token is bound to account: the account on the context, by id and by name, the
// way the auth gate places it, and the label, kind, and account id the handler stamps. Two teams
// can each mint a token with that label, so the account is what tells their runs apart.
func boundAgent(account string) (context.Context, []run.SubmitOption) {
	id := "usr_" + account
	ctx := run.WithAccountContext(context.Background(), id, account)
	return ctx, []run.SubmitOption{run.WithActor("release-agent"),
		run.WithActorType(policy.ActorKindAgent), run.WithActorAccount(id)}
}

// accountRules returns a policy store holding the dev-lead exemption and any further rules.
func accountRules(t *testing.T, more ...*policy.Policy) policy.Store {
	t.Helper()
	exempt := &policy.Policy{ID: "pol_release", Name: releaseOnDevLead, Actor: "release-agent",
		Account: "dev-lead", Effect: policy.EffectExempt, MaxDestroy: policy.DisabledMaxDestroy}
	return rulesHolding(t, append([]*policy.Policy{exempt}, more...)...)
}

// exemptOnDevLead is the note a run the dev-lead exemption let through carries.
var exemptOnDevLead = fmt.Sprintf("requested by an agent bound to account %q, exempt from the "+
	"default hold by policy %q", "dev-lead", releaseOnDevLead)

// TestAnExemptionCoversOneAccountsAgentAndItsDerivedRuns is the label collision the account
// criterion closes. Two agent tokens share the label release-agent, one bound to dev-lead and one
// to ops-lead, and the only exemption names the label on dev-lead. The dev-lead agent's run goes
// ahead, and so do the runs derived from its request: a retry of a failed split with its shards,
// and the apply its plan proposes. The ops-lead agent's identical requests are held by default.
//
//nolint:funlen // Test function.
func TestAnExemptionCoversOneAccountsAgentAndItsDerivedRuns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Account is the account the agent's token is bound to.
		Account string
		// WantHeld is whether its runs wait for a person.
		WantHeld bool
	}{{ // Test 0: The account the exemption names.
		Account: "dev-lead", WantHeld: false,
	}, { // Test 1: The same label on another account, the collision that used to exempt it too.
		Account: "ops-lead", WantHeld: true,
	}}
	// judge checks a run the agent caused: held or not, by what, under which account.
	judge := func(t *testing.T, what string, r *run.Run, account string, wantHeld bool) {
		t.Helper()
		if held := r.Status == run.StatusPendingApproval; held != wantHeld {
			t.Errorf("%s held = %v, want %v (held by %q)", what, held, wantHeld, r.HeldByPolicy)
		}
		if r.Account != account {
			t.Errorf("%s account = %q, want %q", what, r.Account, account)
		}
		want := exemptOnDevLead
		if wantHeld {
			want = policy.AgentDefaultName
		}
		if !slices.Contains(r.PolicyNotes, want) {
			t.Errorf("%s notes = %q, want %q", what, r.PolicyNotes, want)
		}
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d submit", testNum), func(t *testing.T) {
			t.Parallel()
			ctx, opts := boundAgent(test.Account)
			d := newAgentHoldDispatcher(t, run.NewMemStore(), accountRules(t))
			got, err := d.Submit(ctx, "", "", append(opts, run.WithTool(run.ToolBash),
				run.WithCommand("./deploy.sh"))...)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			judge(t, "the run", got, test.Account, test.WantHeld)
		})
		t.Run(fmt.Sprintf("test %d retry", testNum), func(t *testing.T) {
			t.Parallel()
			store := run.NewMemStore()
			d := New(store, &failingLister{hosts: []string{"web01", "web02"}}, zap.NewNop(),
				WithNoJanitor(), WithPolicies(accountRules(t)))
			t.Cleanup(d.Close)
			// A person's split fails, which is what makes a retry available.
			parent, err := d.SubmitSplit(context.Background(), "site.yml", "inv", 2,
				personOpts()...)
			if err != nil {
				t.Fatalf("SubmitSplit() error = %v", err)
			}
			waitForStatus(t, store, parent.ID, run.StatusFailed)
			ctx, opts := boundAgent(test.Account)
			retry, err := d.RetryFailedShards(ctx, parent.ID, opts...)
			if err != nil {
				t.Fatalf("RetryFailedShards() error = %v", err)
			}
			judge(t, "the retry", retry, test.Account, test.WantHeld)
			shards, err := store.Shards(context.Background(), retry.ID)
			if err != nil {
				t.Fatalf("Shards() error = %v", err)
			}
			for _, shard := range shards {
				if shard.Account != test.Account {
					t.Errorf("shard %s account = %q, want %q", shard.ID, shard.Account, test.Account)
				}
			}
		})
		t.Run(fmt.Sprintf("test %d apply", testNum), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"main.tf": cleanConfig})
			store := run.NewMemStore()
			// A plan-content rule plans every apply first, the exempt one included, so the apply
			// its plan proposes faces the rules on its own.
			planned := &policy.Policy{ID: "pol_teardown", Name: "large teardown",
				Tool: run.ToolTerraform, MaxDestroy: 5}
			d := New(store, planRunner("Plan: 0 to add, 0 to change, 1 to destroy"), zap.NewNop(),
				WithNoJanitor(), WithPolicies(accountRules(t, planned)))
			t.Cleanup(d.Close)
			ctx, opts := boundAgent(test.Account)
			created, err := d.Submit(ctx, "", "", append(opts, run.WithTool(run.ToolTerraform),
				run.WithCommand(dir))...)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if final := waitTerminal(t, store, created.ID); final.Status != run.StatusSucceeded {
				t.Fatalf("plan status = %q, want succeeded", final.Status)
			}
			proposal := waitProposal(t, store, created.ID)
			judge(t, "the proposed apply", proposal, test.Account, test.WantHeld)
		})
	}
}

// TestAnAccountIsNeverBorrowed holds stamping the account to the run the request is about. A run
// whose actor names another account than the request's does not take the request's account name,
// so it is judged as an account nobody named, which no exemption covers. The control is the same
// run naming the request's own account, which the exemption lets through.
func TestAnAccountIsNeverBorrowed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// ActorAccount is the account id the run names.
		ActorAccount string
		// WantAccount is the account name the run ends up carrying.
		WantAccount string
		// WantHeld is whether it waits for a person.
		WantHeld bool
	}{{ // Test 0: The request's own account is stamped, and the exemption covers it.
		ActorAccount: "usr_dev-lead", WantAccount: "dev-lead", WantHeld: false,
	}, { // Test 1: Another account's run does not borrow the name, and fails closed.
		ActorAccount: "usr_somebody-else", WantAccount: "", WantHeld: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx, opts := boundAgent("dev-lead")
			d := newAgentHoldDispatcher(t, run.NewMemStore(), accountRules(t))
			got, err := d.Submit(ctx, "", "", append(opts, run.WithActorAccount(test.ActorAccount),
				run.WithTool(run.ToolBash), run.WithCommand("./deploy.sh"))...)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if got.Account != test.WantAccount {
				t.Errorf("account = %q, want %q", got.Account, test.WantAccount)
			}
			if held := got.Status == run.StatusPendingApproval; held != test.WantHeld {
				t.Errorf("held = %v, want %v (held by %q)", held, test.WantHeld, got.HeldByPolicy)
			}
		})
	}
}

// TestAWorkflowsStepsCarryItsAccount holds a workflow's steps to the account the workflow was
// requested under, so the exemption that let an agent's workflow start covers the steps it runs,
// and a rule scoped to the account sees every step. The control is the account on the workflow
// itself, which the steps must match.
func TestAWorkflowsStepsCarryItsAccount(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	d := newAgentHoldDispatcher(t, store, accountRules(t))
	ctx, opts := boundAgent("dev-lead")
	got, err := d.SubmitPipeline(ctx, "nightly", "", []run.PipelineStep{
		{Name: "build", Tool: run.ToolBash, Command: "make"},
		{Name: "ship", Tool: run.ToolBash, Command: "./deploy"},
	}, opts...)
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	if got.Status == run.StatusPendingApproval || got.Account != "dev-lead" {
		t.Fatalf("workflow = %s on %q, want it exempt on dev-lead", got.Status, got.Account)
	}
	if final := waitTerminal(t, store, got.ID); final.Status != run.StatusSucceeded {
		t.Fatalf("workflow status = %q, want succeeded", final.Status)
	}
	steps, err := store.Shards(context.Background(), got.ID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("got %d steps, want 2", len(steps))
	}
	for _, step := range steps {
		if step.Account != "dev-lead" {
			t.Errorf("step %s account = %q, want dev-lead", step.StepName, step.Account)
		}
	}
}
