package dispatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/plantest"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// savedPlanBytes is the plan file the saved plan tests' runner returns from a plan.
var savedPlanBytes = []byte("saved plan the approver was shown")

// savedPlanRunner stands in for Terraform: a plan returns result with a fresh copy of its plan
// file, and an apply records the plan file it was handed, printing notice and exiting with
// exitCode.
type savedPlanRunner struct {
	// result is what a plan returns.
	result roundhouse.Result
	// notice is what an apply prints.
	notice string
	// exitCode is what an apply exits with.
	exitCode int
	// mu guards result and applied.
	mu sync.Mutex
	// applied holds the plan file each apply was handed, empty for an apply handed none.
	applied []string
}

// Run plans or applies.
func (s *savedPlanRunner) Run(_ context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
	if spec.DryRun {
		if spec.PlanOut == "" {
			return roundhouse.Result{ExitCode: 1}, errors.New("the plan was given nowhere to save its file")
		}
		s.mu.Lock()
		res := s.result
		res.PlanFile = slices.Clone(s.result.PlanFile)
		res.PlanJSON = slices.Clone(s.result.PlanJSON)
		s.mu.Unlock()
		return res, nil
	}
	plan := ""
	if spec.PlanFile != "" {
		b, err := os.ReadFile(spec.PlanFile)
		if err != nil {
			return roundhouse.Result{ExitCode: 1}, err
		}
		plan = string(b)
	}
	s.mu.Lock()
	s.applied = append(s.applied, plan)
	s.mu.Unlock()
	_, _ = io.WriteString(out, s.notice)
	return roundhouse.Result{ExitCode: s.exitCode}, nil
}

// applies returns the plan file each apply was handed.
func (s *savedPlanRunner) applies() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.applied)
}

// driftResult is a plan that found drift and saved its plan file.
func driftResult() roundhouse.Result {
	return roundhouse.Result{ExitCode: 0, Drift: true, PlanFile: savedPlanBytes,
		PlanJSON: plantest.JSON(0)}
}

// TestADriftCheckKeepsItsPlanForAReconcile pins what a Terraform drift check leaves behind. A check
// that found drift keeps the plan file it saved, sealed under the server's key, so the reconcile
// proposed from it can carry out exactly that plan. A check that found nothing keeps nothing, a
// check that could not save its plan says a reconcile cannot follow, and none of them binds a plan
// into its own spec, which was settled when the check was submitted.
func TestADriftCheckKeepsItsPlanForAReconcile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// Result is what the check's plan returns.
		Result roundhouse.Result
		// WantKept reports whether the check keeps a plan.
		WantKept bool
		// WantWarning is what the check's record must warn, empty for nothing.
		WantWarning string
	}{{ // Test 0: Drift keeps the saved plan.
		Name: "drift", Result: driftResult(), WantKept: true,
	}, { // Test 1: A check that found nothing keeps nothing.
		Name: "in sync", Result: roundhouse.Result{ExitCode: 0, PlanFile: savedPlanBytes},
	}, { // Test 2: Drift with no plan file saved says a reconcile cannot follow.
		Name: "no plan file", Result: roundhouse.Result{ExitCode: 0, Drift: true},
		WantWarning: "saved no plan file, so a reconcile cannot be proposed from it",
	}, { // Test 3: A failed check keeps nothing.
		Name: "failed", Result: roundhouse.Result{ExitCode: 1, PlanFile: savedPlanBytes},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			d := New(store, &savedPlanRunner{result: test.Result}, nil,
				WithCredentials(credential.NewMemStore(), snapshotSealer), WithRunFilesRoot(t.TempDir()),
				WithNoJanitor())
			t.Cleanup(d.Close)
			check, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform),
				run.WithCommand("infra/network"), run.WithDryRun(true))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			done := waitTerminal(t, store, check.ID)
			sealed, err := store.DriftPlan(ctx, check.ID)
			if err != nil {
				t.Fatalf("DriftPlan() error = %v", err)
			}
			if got := sealed != ""; got != test.WantKept {
				t.Fatalf("plan kept = %v, want %v", got, test.WantKept)
			}
			if test.WantKept {
				if strings.HasPrefix(sealed, plainPrefix) {
					t.Error("the kept plan is stored plain on an install that holds a key")
				}
				plan, err := d.openBytes(sealed)
				if err != nil || !bytes.Equal(plan, savedPlanBytes) {
					t.Errorf("the kept plan opens to %q, %v, want the plan the check saved", plan, err)
				}
			}
			if done.PlanSHA256 != "" || done.PlanSealed != "" {
				t.Errorf("the check binds a plan into its own spec: digest %q", done.PlanSHA256)
			}
			if test.WantWarning != "" && !strings.Contains(done.Warning, test.WantWarning) {
				t.Errorf("warning = %q, want it to say %q", done.Warning, test.WantWarning)
			}
		})
	}
}

// TestARuleHeldApplyPlansFirstAndHoldsItsPlan pins the hold an approval rule puts on a Terraform
// apply. The request is not held, because an approval of it would release a plan made only when it
// ran. It is planned first, and the apply the plan proposes is held by the same rule, carrying the
// saved plan with its digest bound, so the approver releases the plan that applies. Once approved
// the apply carries out that plan file.
func TestARuleHeldApplyPlansFirstAndHoldsItsPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	policies := policy.NewMemStore()
	rule := policy.NewPolicy("terraform needs a person")
	rule.Tool, rule.RequireDistinctApprover = run.ToolTerraform, true
	if err := policies.Save(ctx, rule); err != nil {
		t.Fatalf("Save(policy) error = %v", err)
	}
	store := run.NewMemStore()
	runner := &savedPlanRunner{result: driftResult()}
	d := New(store, runner, nil, WithPolicies(policies), WithAudits(audit.NewMemStore()),
		WithCredentials(credential.NewMemStore(), snapshotSealer), WithRunFilesRoot(t.TempDir()),
		WithNoJanitor())
	t.Cleanup(d.Close)

	plan, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform), run.WithCommand("infra/prod"),
		run.WithActor("operator-1"), run.WithActorAccount("usr_operator"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if plan.Status != run.StatusPending || plan.HeldByPolicy != "" {
		t.Fatalf("the request is %q held by %q, want it queued to plan first", plan.Status,
			plan.HeldByPolicy)
	}
	if done := waitTerminal(t, store, plan.ID); done.Status != run.StatusSucceeded {
		t.Fatalf("plan status = %q (%s), want succeeded", done.Status, done.Error)
	}
	proposal := waitProposal(t, store, plan.ID)
	held, err := store.Get(ctx, proposal.ID)
	if err != nil {
		t.Fatalf("Get(proposal) error = %v", err)
	}
	if held.Status != run.StatusPendingApproval || held.HeldByPolicy != "terraform needs a person" ||
		!held.RequireDistinctApprover {
		t.Fatalf("proposal = %q held by %q distinct %v, want held by the rule with its second "+
			"approver", held.Status, held.HeldByPolicy, held.RequireDistinctApprover)
	}
	if held.PlanSealed == "" || held.PlanSHA256 != run.SealedBlobSHA256(held.PlanSealed) {
		t.Fatalf("the held apply binds no saved plan: digest %q", held.PlanSHA256)
	}
	if got := runner.applies(); len(got) != 0 {
		t.Fatalf("an apply ran before the approval: %q", got)
	}
	if _, err := d.Approve(ctx, held.ID, decider("approver-1", "session")); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	if final := waitTerminal(t, store, held.ID); final.Status != run.StatusSucceeded {
		t.Fatalf("apply status = %q (%s), want succeeded", final.Status, final.Error)
	}
	if diff := cmp.Diff([]string{string(savedPlanBytes)}, runner.applies()); diff != "" {
		t.Errorf("applied plan mismatch (-want +got):\n%s", diff)
	}
}

// TestAStaleSavedPlanSaysTheProposalHasToBeMadeAgain pins what an apply records when the tool
// refuses its saved plan as stale. Nothing was applied and the plan cannot be carried out any more,
// so the run says so and says how to make the proposal again: a reconcile from a new drift check,
// and any other apply by submitting it again. A failure for another reason is left as it was.
func TestAStaleSavedPlanSaysTheProposalHasToBeMadeAgain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// Source is what fired the apply.
		Source string
		// Notice is what the tool prints.
		Notice string
		// WantError is the failure the apply records.
		WantError string
	}{{ // Test 0: A reconcile is proposed again from a new drift check.
		Name: "reconcile", Source: "reconcile",
		Notice:    "\nError: Saved plan is stale\n\nThe given plan file can no longer be applied.\n",
		WantError: "Run the drift check again and propose the reconcile again.",
	}, { // Test 1: A gated apply is submitted again.
		Name: "gated apply", Source: "api",
		Notice:    "\nError: Saved plan is stale\n",
		WantError: "Submit the apply again to plan it afresh and propose it again.",
	}, { // Test 2: Any other failure keeps its own record.
		Name: "other failure", Notice: "Error: provider crashed\n",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			d := New(store, &savedPlanRunner{notice: test.Notice, exitCode: 1}, nil,
				WithCredentials(credential.NewMemStore(), snapshotSealer), WithRunFilesRoot(t.TempDir()),
				WithNoJanitor())
			t.Cleanup(d.Close)
			sealed, err := d.sealBytes(savedPlanBytes)
			if err != nil {
				t.Fatalf("sealBytes() error = %v", err)
			}
			opts := []run.SubmitOption{run.WithTool(run.ToolTerraform), run.WithCommand("infra/prod"),
				run.WithProposedFrom("run_plan"), run.WithPlanFile(sealed)}
			if test.Source != "" {
				opts = append(opts, run.WithSource(test.Source, "run_plan"))
			}
			apply, err := d.Submit(ctx, "", "", opts...)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			final := waitTerminal(t, store, apply.ID)
			if final.Status != run.StatusFailed {
				t.Fatalf("status = %q, want failed", final.Status)
			}
			if test.WantError == "" {
				if strings.Contains(final.Error, "stale") {
					t.Errorf("a failure for another reason reads as a stale plan: %q", final.Error)
				}
				return
			}
			if !strings.HasPrefix(final.Error, "the saved plan is stale") ||
				!strings.HasSuffix(final.Error, test.WantError) {
				t.Errorf("error = %q, want the stale plan named and %q", final.Error, test.WantError)
			}
		})
	}
}

// TestStaleWatchSeesTheNoticeAcrossWrites pins the watch on an apply's output. The tool's notice
// can arrive split across two writes, and a notice in an apply that succeeded, or one whose runner
// failed on its own, is not a refusal of the plan.
func TestStaleWatchSeesTheNoticeAcrossWrites(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Writes are the chunks of output in order.
		Writes []string
		// ExitCode is how the apply exited.
		ExitCode int
		// RunErr is the runner's own failure.
		RunErr error
		// Want reports whether the apply was refused as stale.
		Want bool
	}{{ // Test 0: The notice in one write.
		Writes: []string{"Error: Saved plan is stale\n"}, ExitCode: 1, Want: true,
	}, { // Test 1: The notice split across writes.
		Writes: []string{"Error: Saved pl", "an is st", "ale\n"}, ExitCode: 1, Want: true,
	}, { // Test 2: No notice.
		Writes: []string{"Error: provider crashed\n"}, ExitCode: 1,
	}, { // Test 3: The text in an apply that succeeded is not a refusal.
		Writes: []string{"Saved plan is stale"}, ExitCode: 0,
	}, { // Test 4: A runner that failed on its own is not a refusal of the plan.
		Writes: []string{"Saved plan is stale"}, ExitCode: 1, RunErr: errors.New("killed"),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			w := &staleWatch{}
			for _, chunk := range test.Writes {
				if n, err := w.Write([]byte(chunk)); n != len(chunk) || err != nil {
					t.Fatalf("Write() = %d, %v, want %d, nil", n, err, len(chunk))
				}
			}
			if got := w.refused(roundhouse.Result{ExitCode: test.ExitCode}, test.RunErr); got != test.Want {
				t.Errorf("refused() = %v, want %v", got, test.Want)
			}
		})
	}
	var none *staleWatch
	if none.refused(roundhouse.Result{ExitCode: 1}, nil) {
		t.Error("a run carrying no saved plan was read as refusing one")
	}
}

// TestARequestThatAsksForApprovalPlansFirst pins the hold a Terraform apply's own submission asks
// for. The request is not held, since approving it would release a plan made only when it ran. It
// plans first, records that it asked, and the apply its plan proposes is held carrying the saved
// plan, whatever the rules say and with no rules in force at all, so the approver approves the plan
// that applies. A rule that holds the apply as well names itself, and a second approver the request
// or the rule asked for travels with the hold. A run of any other tool keeps the hold it asked for.
func TestARequestThatAsksForApprovalPlansFirst(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// Rules is the rule store, nil for an install that holds none.
		Rules func(t *testing.T) policy.Store
		// Distinct asks for a second approver on the request itself.
		Distinct bool
		// WantHeldBy is what the held apply names as its reason.
		WantHeldBy string
		// WantDistinct reports whether the held apply needs a second approver.
		WantDistinct bool
	}{{ // Test 0: No rule store at all.
		Name: "no rules", WantHeldBy: holdRequested,
	}, { // Test 1: Rules that do not cover the apply.
		Name: "rules for another tool", Rules: func(t *testing.T) policy.Store {
			t.Helper()
			rules := policy.NewMemStore()
			other := policy.NewPolicy("bash needs a person")
			other.Tool = run.ToolBash
			if err := rules.Save(context.Background(), other); err != nil {
				t.Fatalf("Save(policy) error = %v", err)
			}
			return rules
		},
		Distinct: true, WantHeldBy: holdRequested, WantDistinct: true,
	}, { // Test 2: A rule that holds the apply too names itself and brings its second approver.
		Name: "a rule holds it too", Rules: func(t *testing.T) policy.Store {
			t.Helper()
			rules := policy.NewMemStore()
			hold := policy.NewPolicy("terraform needs a person")
			hold.Tool, hold.RequireDistinctApprover = run.ToolTerraform, true
			if err := rules.Save(context.Background(), hold); err != nil {
				t.Fatalf("Save(policy) error = %v", err)
			}
			return rules
		},
		WantHeldBy: "terraform needs a person", WantDistinct: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			runner := &savedPlanRunner{result: driftResult()}
			opts := []Option{WithAudits(audit.NewMemStore()),
				WithCredentials(credential.NewMemStore(), snapshotSealer), WithRunFilesRoot(t.TempDir()),
				WithNoJanitor()}
			if test.Rules != nil {
				opts = append(opts, WithPolicies(test.Rules(t)))
			}
			d := New(store, runner, nil, opts...)
			t.Cleanup(d.Close)
			plan, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform),
				run.WithCommand("infra/prod"), run.WithRequireApproval(true),
				run.WithRequireDistinctApprover(test.Distinct), run.WithActor("operator-1"),
				run.WithActorAccount("usr_operator"))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if plan.Status != run.StatusPending || !plan.ApprovalRequested || plan.HeldByPolicy != "" {
				t.Fatalf("the request is %q held by %q asked %v, want it queued to plan first with "+
					"its ask recorded", plan.Status, plan.HeldByPolicy, plan.ApprovalRequested)
			}
			if done := waitTerminal(t, store, plan.ID); done.Status != run.StatusSucceeded {
				t.Fatalf("plan status = %q (%s), want succeeded", done.Status, done.Error)
			}
			held, err := store.Get(ctx, waitProposal(t, store, plan.ID).ID)
			if err != nil {
				t.Fatalf("Get(proposal) error = %v", err)
			}
			if held.Status != run.StatusPendingApproval || held.HeldByPolicy != test.WantHeldBy ||
				held.RequireDistinctApprover != test.WantDistinct {
				t.Fatalf("proposal = %q held by %q distinct %v, want held by %q distinct %v",
					held.Status, held.HeldByPolicy, held.RequireDistinctApprover, test.WantHeldBy,
					test.WantDistinct)
			}
			if held.PlanSealed == "" || held.PlanSHA256 != run.SealedBlobSHA256(held.PlanSealed) {
				t.Fatalf("the held apply binds no saved plan: digest %q", held.PlanSHA256)
			}
			if got := runner.applies(); len(got) != 0 {
				t.Fatalf("an apply ran before the approval: %q", got)
			}
			if _, err := d.Approve(ctx, held.ID, decider("approver-1", "session")); err != nil {
				t.Fatalf("Approve() error = %v", err)
			}
			if final := waitTerminal(t, store, held.ID); final.Status != run.StatusSucceeded {
				t.Fatalf("apply status = %q (%s), want succeeded", final.Status, final.Error)
			}
			if diff := cmp.Diff([]string{string(savedPlanBytes)}, runner.applies()); diff != "" {
				t.Errorf("applied plan mismatch (-want +got):\n%s", diff)
			}
		})
	}

	// A run of another tool keeps the hold its submission asked for.
	d := New(run.NewMemStore(), &savedPlanRunner{}, nil, WithNoJanitor())
	t.Cleanup(d.Close)
	other, err := d.Submit(context.Background(), "", "", run.WithTool(run.ToolBash),
		run.WithCommand("deploy prod"), run.WithRequireApproval(true))
	if err != nil {
		t.Fatalf("Submit(bash) error = %v", err)
	}
	if other.Status != run.StatusPendingApproval || other.ApprovalRequested {
		t.Errorf("a bash request is %q asked %v, want it held at submission as it asked",
			other.Status, other.ApprovalRequested)
	}
}

// TestACleanCheckShowsTheTargetInSync pins what the Drift page shows for a Terraform working
// directory. A check that finds drift records it, a later check that finds nothing records the
// directory in sync, so the page shows where it stands now and offers no reconcile for it, and a
// check that fails observed nothing and leaves the last reading in place.
func TestACleanCheckShowsTheTargetInSync(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	runner := &savedPlanRunner{}
	d := New(store, runner, nil, WithCredentials(credential.NewMemStore(), snapshotSealer),
		WithRunFilesRoot(t.TempDir()), WithNoJanitor())
	t.Cleanup(d.Close)
	check := func(result roundhouse.Result) *run.Run {
		t.Helper()
		runner.mu.Lock()
		runner.result = result
		runner.mu.Unlock()
		r, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform),
			run.WithCommand("infra/network"), run.WithDryRun(true))
		if err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
		return waitTerminal(t, store, r.ID)
	}
	drift := func() run.HostDrift {
		t.Helper()
		rows, err := store.DriftStatus(ctx)
		if err != nil || len(rows) != 1 {
			t.Fatalf("DriftStatus() = %+v, %v, want the one directory", rows, err)
		}
		return rows[0]
	}

	drifted := check(driftResult())
	if got := drift(); got.RunID != drifted.ID || got.DriftedTasks == 0 {
		t.Fatalf("after a drifted check the page shows %+v, want it drifted from %s", got, drifted.ID)
	}
	clean := check(roundhouse.Result{ExitCode: 0, PlanFile: savedPlanBytes})
	if got := drift(); got.RunID != clean.ID || got.DriftedTasks != 0 {
		t.Errorf("after a clean check the page shows %+v, want it in sync from %s", got, clean.ID)
	}
	if kept, err := store.DriftPlan(ctx, drifted.ID); err != nil || kept != "" {
		t.Errorf("the drifted check still keeps a plan after the clean one: %q, %v", kept, err)
	}
	check(roundhouse.Result{ExitCode: 1})
	if got := drift(); got.RunID != clean.ID {
		t.Errorf("a failed check moved the page to %+v, want the clean reading kept", got)
	}
}
