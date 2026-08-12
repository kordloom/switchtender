package dispatch

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// heredocPlanFixture is real "tofu plan -destroy -no-color" output for a configuration whose
// terraform_data resource carries a multi-line string attribute. OpenTofu renders that attribute as
// an indented heredoc inside the resource diff, so the captured log holds a line reading
// "Plan: 0 to add, 0 to change, 0 to destroy." above the plan's genuine column-zero summary of two
// destroys.
const heredocPlanFixture = "testdata/tofu-plan-destroy-heredoc.txt"

// readHeredocPlan returns the recorded plan output, failing the test when the fixture is missing or
// no longer contains both the indented decoy and the genuine summary the defect turned on.
func readHeredocPlan(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(heredocPlanFixture)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", heredocPlanFixture, err)
	}
	out := string(b)
	if !strings.Contains(out, "\n            Plan: 0 to add, 0 to change, 0 to destroy.") {
		t.Fatalf("fixture lost its indented heredoc summary, so it no longer covers the defect")
	}
	if !strings.Contains(out, "\nPlan: 0 to add, 0 to change, 2 to destroy.") {
		t.Fatalf("fixture lost its column-zero summary, so it no longer covers the defect")
	}
	return out
}

// TestParsePlanDestroysIgnoresIndentedSummary pins that only a column-zero summary line is the plan's
// verdict, and that column-zero summaries which contradict each other leave the plan unreadable.
//
// The pattern accepted "^[ \t]*Plan:" and the parse took the leftmost match. A heredoc attribute
// inside a resource diff prints an indented line of exactly that shape, so real OpenTofu output for a
// plan destroying two resources parsed as destroys 0 with read true. The gate then weighed zero
// against the limit, queued the apply with nobody asked, and wrote "plan would destroy 0 resource(s)"
// into the run's evidence.
func TestParsePlanDestroysIgnoresIndentedSummary(t *testing.T) {
	t.Parallel()
	realPlan := readHeredocPlan(t)
	tests := []struct {
		Name        string
		In          string
		WantDestroy int
		WantRead    bool
	}{{ // Test 0: Real tofu output whose heredoc decoy understates the two genuine destroys.
		Name:        "heredoc decoy in real tofu output",
		In:          realPlan,
		WantDestroy: 2, WantRead: true,
	}, { // Test 1: An indented summary with no column-zero summary anywhere is not a verdict at all.
		Name:        "only an indented summary",
		In:          "  - input = <<-EOT\n        Plan: 0 to add, 0 to change, 0 to destroy.\n    EOT\n",
		WantDestroy: 0, WantRead: false,
	}, { // Test 2: Two column-zero summaries that disagree leave the real effect unknown.
		Name: "disagreeing column-zero summaries",
		In: "Plan: 0 to add, 0 to change, 0 to destroy.\n" +
			"Plan: 0 to add, 0 to change, 9 to destroy.\n",
		WantDestroy: 0, WantRead: false,
	}, { // Test 3: Repeated identical summaries agree, so the plan is still readable.
		Name: "agreeing column-zero summaries",
		In: "Plan: 1 to add, 0 to change, 4 to destroy.\n" +
			"Plan: 1 to add, 0 to change, 4 to destroy.\n",
		WantDestroy: 4, WantRead: true,
	}, { // Test 4: A tab-indented summary is quoted content too, not the plan's own line.
		Name:        "tab indented summary",
		In:          "\tPlan: 0 to add, 0 to change, 0 to destroy.\n",
		WantDestroy: 0, WantRead: false,
	}, { // Test 5: Summaries agreeing on destroys but not on the rest still leave the plan unknown,
		// since the Drift page counts every clause and cannot be told which line to believe.
		Name: "summaries agree on destroys only",
		In: "Plan: 1 to add, 0 to change, 2 to destroy.\n" +
			"Plan: 5 to add, 0 to change, 2 to destroy.\n",
		WantDestroy: 0, WantRead: false,
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

// TestParsePlanChangesIgnoresIndentedSummary pins that the Drift page counts the plan's own summary
// rather than a heredoc line quoted inside a resource diff.
func TestParsePlanChangesIgnoresIndentedSummary(t *testing.T) {
	t.Parallel()
	if got := parsePlanChanges(readHeredocPlan(t)); got != 2 {
		t.Errorf("parsePlanChanges(real destroy plan) = %d, want 2", got)
	}
}

// TestPlanGateHoldsHeredocPlan pins the whole gate against real OpenTofu output whose resource diff
// quotes a plan summary. The plan destroys two resources under a limit of one, so the proposed apply
// must be held for a person, must not run, and the evidence written under the plan must not tell the
// approver the plan destroys nothing.
func TestPlanGateHoldsHeredocPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	planOut := readHeredocPlan(t)

	store := run.NewMemStore()
	policies := policy.NewMemStore()
	if err := policies.Save(ctx, &policy.Policy{
		ID: policy.NewID(), Name: "tf-destroy-guard", Tool: run.ToolOpenTofu, MaxDestroy: 1,
	}); err != nil {
		t.Fatalf("policies.Save() error = %v", err)
	}
	runner := &planGateRunner{summary: planOut}
	d := New(store, runner, nil, WithPolicies(policies))
	defer d.Close()

	created, err := d.Submit(ctx, "", "",
		run.WithTool(run.ToolOpenTofu), run.WithCommand("infra/prod"))
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
	if diff := cmp.Diff(run.StatusPendingApproval, stored.Status, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("proposed apply status mismatch (-want +got):\n%s", diff)
	}
	wantHeld := "tf-destroy-guard (plan destroys 2, limit 1)"
	if diff := cmp.Diff(wantHeld, stored.HeldByPolicy, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("held-by reason mismatch (-want +got):\n%s", diff)
	}

	// The evidence an approver reads must state the real destroy count, never the decoy's zero.
	logged, err := store.Log(ctx, created.ID)
	if err != nil {
		t.Fatalf("Log(plan run) error = %v", err)
	}
	note := string(logged)
	wantNote := fmt.Sprintf("switchtender: plan would destroy 2 resource(s); proposed apply %s "+
		"held for approval.\n", proposal.ID)
	if !strings.Contains(note, wantNote) {
		t.Errorf("plan evidence missing %q\ngot tail:\n%s", wantNote, tailOf(note))
	}
	if strings.Contains(note, "plan would destroy 0 resource(s)") {
		t.Errorf("plan evidence understates the destroy count as zero:\n%s", tailOf(note))
	}

	// A held apply must not have touched the infrastructure while it waits for a person.
	time.Sleep(200 * time.Millisecond)
	if n := runner.applies.Load(); n != 0 {
		t.Errorf("%d applies executed on a plan held for approval", n)
	}
}

// tailOf returns the last few lines of s, so a failure prints the synthesized decision rather than
// the whole plan output above it.
func tailOf(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 4 {
		lines = lines[len(lines)-4:]
	}
	return strings.Join(lines, "\n")
}
