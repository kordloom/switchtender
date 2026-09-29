package dispatch

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

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

// Run writes the plan summary for a dry run and records a real apply otherwise.
func (p *planGateRunner) Run(_ context.Context, spec roundhouse.Spec,
	out io.Writer) (roundhouse.Result, error) {
	if spec.DryRun {
		_, _ = io.WriteString(out, p.summary)
		return roundhouse.Result{ExitCode: 0, Drift: true}, nil
	}
	p.applies.Add(1)
	_, _ = io.WriteString(out, "Apply complete!\n")
	return roundhouse.Result{ExitCode: 0}, nil
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
	}{{ // Test 0: An import clause must not hide the five destroys from a limit of three.
		Name:         "import clause",
		Summary:      "Plan: 2 to import, 3 to add, 1 to change, 5 to destroy.\n",
		WantApproval: true,
		WantHeld:     "tf-destroy-guard (plan destroys 5, limit 3)",
	}, { // Test 1: A plan with nothing to do queues without approval, as every drift check does.
		Name:         "no changes",
		Summary:      "No changes. Your infrastructure matches the configuration.\n",
		WantApproval: false,
		WantHeld:     "",
	}, { // Test 2: A summary that cannot be read was never weighed, so the apply waits.
		Name:         "unreadable",
		Summary:      "Terraform emitted something this parser does not know.\n",
		WantApproval: true,
		WantHeld: "plan summary unreadable, so the destroy count was never weighed " +
			"against the limit",
	}, { // Test 3: A readable plan under the limit still queues and applies.
		Name:         "under the limit",
		Summary:      "Plan: 1 to import, 0 to add, 0 to change, 2 to destroy.\n",
		WantApproval: false,
		WantHeld:     "",
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
