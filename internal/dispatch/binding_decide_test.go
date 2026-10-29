package dispatch

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// decideRequester is who submits the work under decision.
const decideRequester = "requester"

// decideInstall is one install a decision is made on: a dispatcher, the runs table as a writer who
// holds no key can change it, and the chain.
type decideInstall struct {
	// store is the runs table.
	store *bndRows
	// chain is the audit table.
	chain *bndChain
	// d decides, and runs the steps of a workflow until it waits at its approval step.
	d *Dispatcher
}

// newDecideInstall starts a dispatcher serving the default queue, with a binding key when keyed is
// set.
func newDecideInstall(t *testing.T, keyed bool) *decideInstall {
	t.Helper()
	in := &decideInstall{store: newBndRows(), chain: &bndChain{Store: audit.NewMemStore()}}
	opts := []Option{WithNoJanitor(), WithAudits(in.chain), WithClaimInterval(5 * time.Millisecond)}
	if keyed {
		opts = append(opts, WithBindingKey(bndKey(t)))
	}
	in.d = New(in.store, dispatchtest.OKRunner(), nil, opts...)
	t.Cleanup(in.d.Close)
	return in
}

// heldRun submits a run held for a distinct approver to a queue nobody serves, so an approval
// releases it without running it.
func (in *decideInstall) heldRun(t *testing.T) *run.Run {
	t.Helper()
	r, err := in.d.Submit(context.Background(), "", "", run.WithTool(run.ToolBash),
		run.WithCommand("deploy"), run.WithQueue("parked"), run.WithActor(decideRequester),
		run.WithRequireApproval(true), run.WithRequireDistinctApprover(true))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if r.Status != run.StatusPendingApproval {
		t.Fatalf("submitted run is %s, want pending_approval", r.Status)
	}
	return r
}

// waitingStep submits a workflow that requires a distinct approver, approving it as a whole first
// when approved is set, and returns it and its approval step once it waits there. A coordinator
// registered for the workflow keeps a decision from resuming it.
func (in *decideInstall) waitingStep(t *testing.T, approved bool) (*run.Run, *run.Run) {
	t.Helper()
	ctx := context.Background()
	parent, err := in.d.SubmitPipeline(ctx, "release", "", gatedWorkflow(0, false),
		run.WithActor(decideRequester), run.WithRequireDistinctApprover(true),
		run.WithRequireApproval(approved))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	if approved {
		if _, err := in.d.Approve(ctx, parent.ID, bndDecider()); err != nil {
			t.Fatalf("Approve() error = %v", err)
		}
	}
	node := waitParked(t, in.store.Store, parent.ID)
	if !node.RequireDistinctApprover {
		t.Fatal("the approval step does not carry its workflow's distinct-approver rule")
	}
	t.Cleanup(in.d.enterCoordinator(parent.ID))
	return parent, node
}

// decide decides target as by, approving when approve is set, through the calls a person's
// decision reaches: Approve or Reject for a held run, and DecideStep for an approval step of
// parent.
func (in *decideInstall) decide(parent, target *run.Run, by string, approve bool) error {
	ctx := context.Background()
	who := outcome.Decider{Name: by, Type: "session"}
	var err error
	switch {
	case parent != nil:
		_, err = in.d.DecideStep(ctx, target.ID, StepDecision{Approve: approve, By: who})
	case approve:
		_, err = in.d.Approve(ctx, target.ID, who)
	default:
		_, err = in.d.Reject(ctx, target.ID, "", who)
	}
	return err
}

// verdicts counts the DECISION entries on the chain that approve or reject target, an approval
// step of parent when parent is set.
func (in *decideInstall) verdicts(t *testing.T, parent, target *run.Run) int {
	t.Helper()
	chain, err := in.chain.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	paths := map[string]bool{}
	for _, verdict := range []string{outcome.StepApproved, outcome.StepRejected} {
		if parent == nil {
			paths["/runs/"+target.ID+"/decision/"+verdict] = true
			continue
		}
		paths[outcome.StepDecisionPath(parent.ID, target.ID, verdict)] = true
	}
	count := 0
	for _, e := range chain {
		if e.Method == audit.MethodDecision && paths[e.Path] {
			count++
		}
	}
	return count
}

// decideResult is what a decision under test did.
type decideResult struct {
	// Refused reports whether the decision's error starts with the refusal's prefix.
	Refused bool
	// Unverified reports whether the decision's error says the row could not be verified, without
	// the store's own words.
	Unverified bool
	// Verdicts counts the DECISION entries that approve or reject the row.
	Verdicts int
	// Decided reports whether the row stores a decision.
	Decided bool
	// Status is the row's status afterward.
	Status run.Status
}

// TestADecisionRefusesARewrittenHeldRow pins that a decision on a held run or on a waiting
// workflow approval step is made only on the row as this process created it. A writer who holds no
// key clears the separation-of-duties flag, or another field the seal covers, and the requester
// then decides their own work. On a keyed dispatcher the decision is refused before anything is
// recorded: no decision entry, no decision on the row, and the row still waiting. Without the
// check, the approval also sealed the cleared flag, so the executor accepted the run. A held run
// whose seal a writer cleared is refused the same way, as carrying no seal, so clearing the seal
// does not turn the check off. An unkeyed dispatcher decides as it always has.
//
//nolint:funlen // Test function.
func TestADecisionRefusesARewrittenHeldRow(t *testing.T) {
	t.Parallel()
	clearDistinct := func(r *run.Run) { r.RequireDistinctApprover = false }
	clearSeal := func(r *run.Run) { r.SpecSeal = "" }
	tests := []struct {
		// Keyed starts the dispatcher with a binding key.
		Keyed bool
		// Step decides a workflow approval step rather than a held run.
		Step bool
		// Approved holds the step's workflow for approval and approves it before it runs.
		Approved bool
		// ChainDown fails every read of a chain entry by id once the row waits for its decision.
		ChainDown bool
		// Rewrite is the writer's change to the row decided, nil for none.
		Rewrite func(r *run.Run)
		// RewriteWorkflow is the writer's change to the step's workflow, nil for none.
		RewriteWorkflow func(r *run.Run)
		// By names the decider.
		By string
		// Approve approves, and false rejects or denies.
		Approve bool
		// WantResult is what the decision did.
		WantResult decideResult
	}{{ // Test 0: An untouched held run approved by another person is released.
		Keyed: true, By: "approver", Approve: true,
		WantResult: decideResult{Verdicts: 1, Decided: true, Status: run.StatusPending},
	}, { // Test 1: A held run whose flag was cleared, approved by its requester, is refused.
		Keyed: true, Rewrite: clearDistinct, By: decideRequester, Approve: true,
		WantResult: decideResult{Refused: true, Status: run.StatusPendingApproval},
	}, { // Test 2: A held run whose rule was cleared is refused to anyone.
		Keyed: true, Rewrite: func(r *run.Run) { r.HeldByPolicy = "" }, By: "approver",
		Approve:    true,
		WantResult: decideResult{Refused: true, Status: run.StatusPendingApproval},
	}, { // Test 3: A held run whose flag was cleared is not rejected either.
		Keyed: true, Rewrite: clearDistinct, By: decideRequester,
		WantResult: decideResult{Refused: true, Status: run.StatusPendingApproval},
	}, { // Test 4: With no key, the requester approves the cleared run as before.
		Rewrite: clearDistinct, By: decideRequester, Approve: true,
		WantResult: decideResult{Verdicts: 1, Decided: true, Status: run.StatusPending},
	}, { // Test 5: An untouched waiting step approved by another person is released.
		Keyed: true, Step: true, By: "approver", Approve: true,
		WantResult: decideResult{Verdicts: 1, Decided: true, Status: run.StatusSucceeded},
	}, { // Test 6: A waiting step whose flag was cleared, approved by the requester, is refused.
		Keyed: true, Step: true, Rewrite: clearDistinct, By: decideRequester, Approve: true,
		WantResult: decideResult{Refused: true, Status: run.StatusPendingApproval},
	}, { // Test 7: A waiting step whose flag was cleared is not denied either.
		Keyed: true, Step: true, Rewrite: clearDistinct, By: decideRequester,
		WantResult: decideResult{Refused: true, Status: run.StatusPendingApproval},
	}, { // Test 8: A waiting step whose workflow was rewritten is refused to anyone.
		Keyed: true, Step: true, By: "approver", Approve: true,
		RewriteWorkflow: func(r *run.Run) { r.ExtraVars = map[string]any{"target": "everywhere"} },
		WantResult:      decideResult{Refused: true, Status: run.StatusPendingApproval},
	}, { // Test 9: A waiting step in an approved workflow approved by another person is released.
		Keyed: true, Step: true, Approved: true, By: "approver", Approve: true,
		WantResult: decideResult{Verdicts: 1, Decided: true, Status: run.StatusSucceeded},
	}, { // Test 10: A chain that cannot be read leaves the step undecided and claims no tampering.
		Keyed: true, Step: true, Approved: true, ChainDown: true, By: "approver", Approve: true,
		WantResult: decideResult{Unverified: true, Status: run.StatusPendingApproval},
	}, { // Test 11: With no key, the requester approves the cleared step as before.
		Step: true, Rewrite: clearDistinct, By: decideRequester, Approve: true,
		WantResult: decideResult{Verdicts: 1, Decided: true, Status: run.StatusSucceeded},
	}, { // Test 12: A held run whose seal was cleared is refused to another person's approval.
		Keyed: true, Rewrite: clearSeal, By: "approver", Approve: true,
		WantResult: decideResult{Refused: true, Status: run.StatusPendingApproval},
	}, { // Test 13: A held run whose seal was cleared is not rejected either.
		Keyed: true, Rewrite: clearSeal, By: "approver",
		WantResult: decideResult{Refused: true, Status: run.StatusPendingApproval},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			in := newDecideInstall(t, test.Keyed)
			var parent, target *run.Run
			if test.Step {
				parent, target = in.waitingStep(t, test.Approved)
			} else {
				target = in.heldRun(t)
			}
			if test.Keyed && target.SpecSeal == "" {
				t.Fatalf("run %s waits for a decision carrying no seal", target.ID)
			}
			if test.Rewrite != nil {
				in.store.rewrite(target.ID, test.Rewrite)
			}
			if test.RewriteWorkflow != nil {
				in.store.rewrite(parent.ID, test.RewriteWorkflow)
			}
			if test.ChainDown {
				in.chain.set(nil, -1)
			}
			err := in.decide(parent, target, test.By, test.Approve)
			stored, gerr := in.store.Store.Get(context.Background(), target.ID)
			if gerr != nil {
				t.Fatalf("Get() error = %v", gerr)
			}
			text := ""
			if err != nil {
				text = err.Error()
			}
			got := decideResult{
				Refused: strings.HasPrefix(text, "refused: "),
				Unverified: strings.HasPrefix(text, "could not verify: ") &&
					!strings.Contains(text, errBndUnreachable.Error()),
				Verdicts: in.verdicts(t, parent, target),
				Decided:  stored.DecisionID != "",
				Status:   stored.Status,
			}
			if diff := cmp.Diff(test.WantResult, got); diff != "" {
				t.Errorf("decision mismatch (-want +got):\n%s\nerror: %v", diff, err)
			}
		})
	}
}
