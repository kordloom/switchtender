package dispatch

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/plantest"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestParsePlanDestroys covers reading the destroy count out of a plan's change summary.
//
// The pattern used to be pinned to "N to add, N to change, N to destroy" in that exact order.
// Terraform 1.5 prints a leading "N to import" clause, which did not match, so the parser reported
// zero destroys and every plan carrying an import sailed past the destroy limit.
func TestParsePlanDestroys(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		In          string
		WantDestroy int
		WantRead    bool
	}{{ // Test 0: The classic three-clause summary.
		Name:        "classic",
		In:          "actions:\n\nPlan: 1 to add, 0 to change, 5 to destroy.\n",
		WantDestroy: 5, WantRead: true,
	}, { // Test 1: Terraform 1.5 prepends an import clause, which must not hide the destroys.
		Name:        "import clause",
		In:          "Plan: 2 to import, 3 to add, 1 to change, 5 to destroy.\n",
		WantDestroy: 5, WantRead: true,
	}, { // Test 2: A plan with nothing to do is a real zero, not an unreadable plan.
		Name:        "no changes",
		In:          "No changes. Your infrastructure matches the configuration.\n",
		WantDestroy: 0, WantRead: true,
	}, { // Test 3: Output with no summary at all leaves the destroy count unknown.
		Name:        "garbage",
		In:          "Error: could not load plugin\n",
		WantDestroy: 0, WantRead: false,
	}, { // Test 4: A summary destroying nothing is read, so it is not held.
		Name:        "zero destroys",
		In:          "Plan: 4 to add, 0 to change, 0 to destroy.\n",
		WantDestroy: 0, WantRead: true,
	}, { // Test 5: An indented "No changes." is content inside a diff, not the plan's own verdict.
		Name:        "no changes quoted inside a diff",
		In:          "Error: boom\n  No changes. blah\n",
		WantDestroy: 0, WantRead: false,
	}, { // Test 6: A plan touching only outputs has no summary line and destroys nothing.
		Name:        "outputs only",
		In:          "Changes to Outputs:\n  + example = \"hello\"\n",
		WantDestroy: 0, WantRead: true,
	}, { // Test 7: A trailing forget clause is another shape the fixed pattern missed.
		Name:        "forget clause",
		In:          "Plan: 0 to add, 0 to change, 2 to destroy, 1 to forget.\n",
		WantDestroy: 2, WantRead: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			gotDestroy, gotRead := parsePlanDestroys(test.In)
			if diff := cmp.Diff(test.WantDestroy, gotDestroy, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("destroys mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantRead, gotRead, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("read mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// planGateRunner emits a fixed plan summary for a plan and counts the applies that really executed,
// so a test can assert whether a gated apply ever reached the infrastructure.
type planGateRunner struct {
	// applies counts executions that were not dry runs, which are the real applies.
	applies atomic.Int64
	// summary is the plan output written whenever a dry run executes.
	summary string
}

// Run writes the plan summary for a dry run, returning the plan it saved, and records a real apply
// otherwise.
func (p *planGateRunner) Run(_ context.Context, spec roundhouse.Spec,
	out io.Writer) (roundhouse.Result, error) {
	if spec.DryRun {
		_, _ = io.WriteString(out, p.summary)
		return plantest.Result(p.summary), nil
	}
	p.applies.Add(1)
	_, _ = io.WriteString(out, "Apply complete!\n")
	return roundhouse.Result{ExitCode: 0}, nil
}

// TestCancelingAPlanGateEndsItCanceledAndProposesNothing pins what a person's cancel does to a
// plan-gated run while its plan executes: the run ends canceled, like any run stopped in flight,
// and no apply is proposed from a plan that never finished. Finalized as failed instead, a plan a
// person stopped reads as a plan that broke, which somebody investigates and a failure channel
// pages on.
func TestCancelingAPlanGateEndsItCanceledAndProposesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	policies := policy.NewMemStore()
	if err := policies.Save(ctx, &policy.Policy{
		ID: policy.NewID(), Name: "tf-destroy-guard", Tool: run.ToolTerraform, MaxDestroy: 0,
	}); err != nil {
		t.Fatalf("policies.Save() error = %v", err)
	}
	store := run.NewMemStore()
	planning := make(chan struct{})
	runner := roundhouse.RunnerFunc(
		func(ctx context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			if !spec.DryRun {
				t.Error("an apply ran although its plan was canceled before it finished")
				return roundhouse.Result{ExitCode: 0}, nil
			}
			close(planning)
			<-ctx.Done()
			return roundhouse.Result{ExitCode: -1}, ctx.Err()
		})
	d := New(store, runner, nil, WithPolicies(policies), WithNoJanitor())
	defer d.Close()

	submitted, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform),
		run.WithCommand("infra/legacy"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	select {
	case <-planning:
	case <-time.After(10 * time.Second):
		t.Fatal("the plan never started, so the run did not go through the plan gate")
	}
	if !d.Cancel(submitted.ID) {
		t.Fatal("Cancel() = false for a plan-gated run whose plan is executing")
	}
	if got := waitTerminal(t, store, submitted.ID); got.Status != run.StatusCanceled {
		t.Errorf("status = %q, want canceled: a plan a person stopped is not a plan that broke",
			got.Status)
	}
	runs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, r := range runs {
		if r.ProposedFrom == submitted.ID {
			t.Errorf("apply %s was proposed from a plan that was canceled", r.ID)
		}
	}
}

// TestPlanGateHoldsImportPlan pins that the destroy limit is enforced on every plan summary shape,
// and that a summary nobody could read holds the apply instead of applying it.
//
// The gate read the destroy count with a pattern fixed to add, change, destroy in that order. A
// Terraform 1.5 plan prints "Plan: 2 to import, 3 to add, 1 to change, 5 to destroy.", which did not
// match, so the count came back zero and a plan destroying five resources under a limit of three was
// queued and applied with nobody asked. The same silence covered any output the parser could not
// read at all, so the gate failed open exactly where it was least able to judge.
func TestPlanGateHoldsImportPlan(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		Summary      string
		WantHeld     string
		WantApproval bool
		// WantNote is what the plan run's log tells the approver the plan found.
		WantNote string
	}{{ // Test 0: An import clause must not hide the five destroys from a limit of three.
		Name:         "import clause",
		Summary:      "Plan: 2 to import, 3 to add, 1 to change, 5 to destroy.\n",
		WantApproval: true,
		WantHeld:     "tf-destroy-guard (plan destroys 5, limit 3)",
		WantNote:     "plan would destroy 5 resource(s)",
	}, { // Test 1: A plan with nothing to do queues without approval, as every drift check does.
		Name:         "no changes",
		Summary:      "No changes. Your infrastructure matches the configuration.\n",
		WantApproval: false,
		WantHeld:     "",
		WantNote:     "plan would destroy 0 resource(s)",
	}, { // Test 2: A summary that cannot be read was never weighed, so the apply waits.
		Name:         "unreadable",
		Summary:      "Terraform emitted something this parser does not know.\n",
		WantApproval: true,
		WantHeld: "plan summary unreadable, so the destroy count was never weighed " +
			"against the limit",
		WantNote: "plan summary could not be read",
	}, { // Test 3: A readable plan under the limit still queues and applies.
		Name:         "under the limit",
		Summary:      "Plan: 1 to import, 0 to add, 0 to change, 2 to destroy.\n",
		WantApproval: false,
		WantHeld:     "",
		WantNote:     "plan would destroy 2 resource(s)",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			policies := policy.NewMemStore()
			if err := policies.Save(ctx, &policy.Policy{
				ID: policy.NewID(), Name: "tf-destroy-guard", Tool: run.ToolTerraform, MaxDestroy: 3,
			}); err != nil {
				t.Fatalf("policies.Save() error = %v", err)
			}
			runner := &planGateRunner{summary: test.Summary}
			d := New(store, runner, nil, WithPolicies(policies))
			defer d.Close()

			created, err := d.Submit(ctx, "", "",
				run.WithTool(run.ToolTerraform), run.WithCommand("infra/prod"))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if plan := waitTerminal(t, store, created.ID); plan.Status != run.StatusSucceeded {
				t.Fatalf("plan run status = %q, want succeeded", plan.Status)
			}

			// The approver reads this note beside the plan. An unreadable summary told as "would destroy
			// 0" would say the plan is harmless when nobody could read it.
			planLog, err := store.Log(ctx, created.ID)
			if err != nil {
				t.Fatalf("Log(plan) error = %v", err)
			}
			if !strings.Contains(string(planLog), test.WantNote) {
				t.Errorf("the plan's log does not say %q; it ends:\n%s", test.WantNote,
					planLog[max(0, len(planLog)-200):])
			}

			proposal := waitProposal(t, store, created.ID)
			stored, err := store.Get(ctx, proposal.ID)
			if err != nil {
				t.Fatalf("Get(proposal) error = %v", err)
			}
			gotApproval := stored.Status == run.StatusPendingApproval
			if gotApproval != test.WantApproval {
				t.Errorf("proposed apply status = %q, want approval required = %v", stored.Status,
					test.WantApproval)
			}
			held := stored.HeldByPolicy
			if diff := cmp.Diff(test.WantHeld, held, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("held-by reason mismatch (-want +got):\n%s", diff)
			}

			if test.WantApproval {
				// A held apply must not have touched anything while it waits for a person.
				time.Sleep(200 * time.Millisecond)
				if n := runner.applies.Load(); n != 0 {
					t.Errorf("%d applies executed on a plan held for approval", n)
				}
				return
			}
			if applied := waitTerminal(t, store, stored.ID); applied.Status != run.StatusSucceeded {
				t.Fatalf("proposed apply status = %q, want succeeded", applied.Status)
			}
			if n := runner.applies.Load(); n != 1 {
				t.Errorf("applies = %d, want 1: an unheld apply did not run", n)
			}
		})
	}
}

// TestCappedBufferHoldsALimitAndSaysWhenItStopped pins the bound on the plan the gate reads.
//
// The whole plan was buffered with no limit while the run's own log is capped, so a large or
// deliberately inflated plan escaped the container's memory limit into the server heap and was copied
// again to be parsed. Several gated applies at once could take the process down, and with it every
// other run, the API, and the UI. The bound has to report that it stopped, because a truncated plan
// must be treated as unweighed rather than judged from the part that fit.
func TestCappedBufferHoldsALimitAndSaysWhenItStopped(t *testing.T) {
	t.Parallel()

	// Under the limit: everything is held and nothing is reported truncated.
	small := &cappedBuffer{cap: 64}
	line := []byte("Plan: 1 to add, 0 to change, 3 to destroy.")
	n, err := small.Write(line)
	if err != nil || n != len(line) {
		t.Fatalf("Write() = %d, %v; want %d and no error", n, err, len(line))
	}
	if small.truncated {
		t.Error("a buffer under its limit reported truncation")
	}
	if !strings.Contains(small.String(), "3 to destroy") {
		t.Errorf("the summary was not held: %q", small.String())
	}

	// Over the limit: bounded, flagged, and still reporting the full write length so the writer it
	// tees from is never told its output was short.
	big := &cappedBuffer{cap: 1024}
	huge := strings.Repeat("x", 5<<20)
	n, err = big.Write([]byte(huge))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if n != len(huge) {
		t.Errorf("Write() reported %d of %d bytes; a short write would break the tee", n, len(huge))
	}
	if !big.truncated {
		t.Error("a buffer past its limit did not report truncation, so a partial plan would be " +
			"weighed as though it were whole")
	}
	if got := len(big.String()); got > 1024 {
		t.Errorf("held %d bytes past a 1024 limit: the plan is not actually bounded", got)
	}

	// Exactly the limit: all of it fits, so it is whole. Reported truncated, a plan that happened
	// to fill the cap to the byte would be held as unreadable.
	exact := &cappedBuffer{cap: 64}
	if _, err := exact.Write(bytes.Repeat([]byte("y"), 64)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if exact.truncated || len(exact.String()) != 64 {
		t.Errorf("a write of exactly the limit held %d bytes and truncated = %v, want 64 and false",
			len(exact.String()), exact.truncated)
	}

	// One byte over in a single write: the limit's worth is kept, not dropped with the overflow.
	over := &cappedBuffer{cap: 64}
	if _, err := over.Write(bytes.Repeat([]byte("z"), 65)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if !over.truncated || len(over.String()) != 64 {
		t.Errorf("a write one byte over held %d bytes and truncated = %v, want 64 and true",
			len(over.String()), over.truncated)
	}
}

// TestPlanGateDistinctApproverComesFromAnyExceededRule pins the plan gate's separation-of-duties
// answer against rule order, the same property the dispatcher's pipeline pass and the policy
// package's rule-list pass both hold.
//
// The gate used to copy the flag from the first rule the destroy count exceeded, so a list holding
// a loose rule without the requirement ahead of a strict rule with it produced a held apply the
// requester could release alone: the stricter rule's second person was dropped by ordering, which
// is never a decision anybody made.
func TestPlanGateDistinctApproverComesFromAnyExceededRule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	policies := policy.NewMemStore()
	// The loose rule first: exceeded, and it asks nothing about who approves.
	if err := policies.Save(ctx, &policy.Policy{
		ID: policy.NewID(), Name: "loose", Tool: run.ToolTerraform, MaxDestroy: 3,
	}); err != nil {
		t.Fatalf("Save(loose) error = %v", err)
	}
	// The strict rule second: also exceeded, and it demands a second person.
	if err := policies.Save(ctx, &policy.Policy{
		ID: policy.NewID(), Name: "strict", Tool: run.ToolTerraform, MaxDestroy: 0,
		RequireDistinctApprover: true,
	}); err != nil {
		t.Fatalf("Save(strict) error = %v", err)
	}
	runner := &planGateRunner{summary: "Plan: 0 to add, 0 to change, 5 to destroy.\n"}
	d := New(store, runner, nil, WithPolicies(policies))
	defer d.Close()

	created, err := d.Submit(ctx, "", "",
		run.WithTool(run.ToolTerraform), run.WithCommand("infra/prod"), run.WithActor("requester"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if plan := waitTerminal(t, store, created.ID); plan.Status != run.StatusSucceeded {
		t.Fatalf("plan run status = %q, want succeeded", plan.Status)
	}
	proposal := waitProposal(t, store, created.ID)
	stored, err := store.Get(ctx, proposal.ID)
	if err != nil {
		t.Fatalf("Get(proposal) error = %v", err)
	}
	if stored.Status != run.StatusPendingApproval {
		t.Fatalf("proposed apply status = %q, want pending_approval", stored.Status)
	}
	if !stored.RequireDistinctApprover {
		t.Fatal("the held apply does not carry the strict rule's distinct-approver requirement: " +
			"rule order dropped the second person")
	}
}
