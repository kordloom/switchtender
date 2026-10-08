package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// commandRecorder is a runner that succeeds and records every command it was handed, so a test can
// say which steps executed and how many times.
type commandRecorder struct {
	// mu guards seen.
	mu sync.Mutex
	// seen counts executions per command.
	seen map[string]int
}

// Run records the command and succeeds.
func (c *commandRecorder) Run(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]int{}
	}
	c.seen[spec.Command]++
	return roundhouse.Result{ExitCode: 0}, nil
}

// count returns how many times command executed.
func (c *commandRecorder) count(command string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen[command]
}

// gatedWorkflow is build, then an approval step, then deploy on approval and notify on denial.
func gatedWorkflow(timeout int, withDenyPath bool) []run.PipelineStep {
	steps := []run.PipelineStep{
		{Name: "build", Tool: run.ToolBash, Command: "build"},
		{Name: "gate", Type: run.StepApproval, Description: "Ship it?", ApprovalTimeout: timeout,
			DependsOn: []string{"build"}},
		{Name: "deploy", Tool: run.ToolBash, Command: "deploy", DependsOn: []string{"gate"}},
	}
	if withDenyPath {
		steps = append(steps, run.PipelineStep{Name: "notify", Tool: run.ToolBash,
			Command: "notify", IfDenied: []string{"gate"}})
	}
	return steps
}

// waitParked waits until the workflow is parked at its approval step and returns the step's record.
func waitParked(t *testing.T, store run.Store, parentID string) *run.Run {
	t.Helper()
	deadline := time.Now().Add(waitBudget)
	for time.Now().Before(deadline) {
		parent, err := store.Get(context.Background(), parentID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		pending, err := run.PendingApprovalSteps(context.Background(), store, parentID)
		if err != nil {
			t.Fatalf("PendingApprovalSteps() error = %v", err)
		}
		if parent.Status == run.StatusPendingApproval && parent.ClaimedBy == "" && len(pending) == 1 {
			return pending[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("workflow %s never parked at its approval step", parentID)
	return nil
}

// stepPaths returns the chain paths of every approval step entry, in chain order.
func stepPaths(t *testing.T, audits audit.Store) []string {
	t.Helper()
	chain, err := audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	var out []string
	for _, e := range chain {
		if _, _, verdict, ok := outcome.ParseStepDecisionPath(e.Path); ok {
			out = append(out, verdict)
		}
	}
	return out
}

// TestApprovalStepPausesAndTakesAPath pins the semantics of an approval step: upstream runs, the
// workflow parks at the step, and the answer decides which path runs. A denial with a deny path is
// the workflow doing what its author wrote, and a denial without one fails it, the way AWX reads a
// node with and without a failure path. A timeout takes the deny path.
func TestApprovalStepPausesAndTakesAPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Decide is approve, deny, or timeout.
		Decide string
		// DenyPath gives the workflow a step that runs on a denial.
		DenyPath bool
		// Timeout is the approval step's timeout in seconds.
		Timeout int
		// WantStatus is the workflow's final status.
		WantStatus run.Status
		// WantDeploy is how many times the approve path ran.
		WantDeploy int
		// WantNotify is how many times the deny path ran.
		WantNotify int
		// WantVerdicts are the step's chain entries in order.
		WantVerdicts []string
	}{{ // Test 0: An approval runs the approve path and skips the deny path.
		Decide: "approve", DenyPath: true, WantStatus: run.StatusSucceeded, WantDeploy: 1,
		WantVerdicts: []string{outcome.StepRequested, outcome.StepApproved},
	}, { // Test 1: A denial runs the deny path, skips the approve path, and succeeds.
		Decide: "deny", DenyPath: true, WantStatus: run.StatusSucceeded, WantNotify: 1,
		WantVerdicts: []string{outcome.StepRequested, outcome.StepRejected},
	}, { // Test 2: A denial with no deny path fails the workflow and runs nothing after the step.
		Decide: "deny", WantStatus: run.StatusFailed,
		WantVerdicts: []string{outcome.StepRequested, outcome.StepRejected},
	}, { // Test 3: A timeout takes the deny path and the chain says the system ended the wait.
		Decide: "timeout", DenyPath: true, Timeout: 1, WantStatus: run.StatusSucceeded, WantNotify: 1,
		WantVerdicts: []string{outcome.StepRequested, outcome.StepTimedOut},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			audits := audit.NewMemStore()
			runner := &commandRecorder{}
			d := New(store, runner, nil, WithNoJanitor(), WithAudits(audits))
			defer d.Close()

			parent, err := d.SubmitPipeline(ctx, "release", "", gatedWorkflow(test.Timeout, test.DenyPath),
				run.WithActor("requester"))
			if err != nil {
				t.Fatalf("SubmitPipeline() error = %v", err)
			}
			node := waitParked(t, store, parent.ID)
			if runner.count("build") != 1 || runner.count("deploy") != 0 {
				t.Fatalf("before the decision build ran %d and deploy %d times, want 1 and 0",
					runner.count("build"), runner.count("deploy"))
			}
			approver := outcome.Decider{Name: "approver", Type: "session"}
			switch test.Decide {
			case "approve":
				_, err = d.DecideStep(ctx, node.ID, StepDecision{Approve: true, By: approver})
			case "deny":
				_, err = d.DecideStep(ctx, node.ID, StepDecision{Reason: "not today", By: approver})
			case "timeout":
				time.Sleep(1100 * time.Millisecond)
				d.sweepApprovalSteps()
			}
			if err != nil {
				t.Fatalf("decide error = %v", err)
			}
			got := waitTerminal(t, store, parent.ID)
			if got.Status != test.WantStatus {
				t.Errorf("workflow status = %q (%s), want %q", got.Status, got.Error, test.WantStatus)
			}
			if runner.count("deploy") != test.WantDeploy || runner.count("notify") != test.WantNotify {
				t.Errorf("deploy ran %d and notify %d times, want %d and %d", runner.count("deploy"),
					runner.count("notify"), test.WantDeploy, test.WantNotify)
			}
			if runner.count("build") != 1 {
				t.Errorf("build ran %d times, want once: resuming must not repeat what already ran",
					runner.count("build"))
			}
			if diff := cmp.Diff(test.WantVerdicts, stepPaths(t, audits)); diff != "" {
				t.Errorf("approval step entries mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestApprovalStepSurvivesARestart pins durability. The process that parked the workflow stops, and
// a different process records the approval and finishes the workflow from what the store holds,
// without running the finished steps again.
func TestApprovalStepSurvivesARestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	audits := audit.NewMemStore()
	first := &commandRecorder{}
	d1 := New(store, first, nil, WithNoJanitor(), WithAudits(audits), WithOwner("replica-one"))
	parent, err := d1.SubmitPipeline(ctx, "release", "", gatedWorkflow(0, true))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	node := waitParked(t, store, parent.ID)
	d1.Close()

	second := &commandRecorder{}
	d2 := New(store, second, nil, WithNoJanitor(), WithAudits(audits), WithOwner("replica-two"))
	defer d2.Close()
	if _, err := d2.DecideStep(ctx, node.ID, StepDecision{Approve: true,
		By: outcome.Decider{Name: "approver", Type: "session"}}); err != nil {
		t.Fatalf("DecideStep() error = %v", err)
	}
	got := waitTerminal(t, store, parent.ID)
	if got.Status != run.StatusSucceeded {
		t.Errorf("workflow status = %q (%s), want succeeded", got.Status, got.Error)
	}
	if first.count("build") != 1 || second.count("build") != 0 {
		t.Errorf("build ran %d times before and %d after the restart, want 1 and 0",
			first.count("build"), second.count("build"))
	}
	if second.count("deploy") != 1 || second.count("notify") != 0 {
		t.Errorf("after the restart deploy ran %d and notify %d times, want 1 and 0",
			second.count("deploy"), second.count("notify"))
	}
}

// TestApprovalStepResumesExactlyOnce pins the HA rule: every replica that sees a decision tries to
// resume the workflow, and exactly one does. Two coordinators would run the approved path twice.
func TestApprovalStepResumesExactlyOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	runner := &commandRecorder{}
	replicas := make([]*Dispatcher, 4)
	for i := range replicas {
		replicas[i] = New(store, runner, nil, WithNoJanitor(),
			WithOwner(fmt.Sprintf("replica-%d", i)))
		defer replicas[i].Close()
	}
	parent, err := replicas[0].SubmitPipeline(ctx, "release", "", gatedWorkflow(0, true))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	node := waitParked(t, store, parent.ID)
	// The decision is recorded directly, as a replica that died before resuming would leave it, so
	// every replica below races to resume the same parked workflow.
	if err := store.StampApprovedSpec(ctx, node.ID, "", ""); err != nil {
		t.Fatalf("StampApprovedSpec() error = %v", err)
	}
	state, err := outcome.StepStateOf(ctx, store, parent, node)
	if err != nil {
		t.Fatalf("StepStateOf() error = %v", err)
	}
	binding, err := state.Binding()
	if err != nil {
		t.Fatalf("Binding() error = %v", err)
	}
	if err := store.StampApprovedSpec(ctx, node.ID, "", binding); err != nil {
		t.Fatalf("StampApprovedSpec() error = %v", err)
	}
	if ok, err := store.SettleHeld(ctx, node.ID, run.Finalization{Status: run.StatusSucceeded,
		EndedAt: time.Now()}); err != nil || !ok {
		t.Fatalf("SettleHeld() = (%v, %v), want (true, nil)", ok, err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	resumed := 0
	for _, d := range replicas {
		for range 5 {
			wg.Add(1)
			go func(d *Dispatcher) {
				defer wg.Done()
				if d.resumeParked(ctx, parent.ID) {
					mu.Lock()
					resumed++
					mu.Unlock()
				}
				d.sweepApprovalSteps()
			}(d)
		}
	}
	wg.Wait()
	got := waitTerminal(t, store, parent.ID)
	if got.Status != run.StatusSucceeded {
		t.Errorf("workflow status = %q (%s), want succeeded", got.Status, got.Error)
	}
	if resumed > 1 {
		t.Errorf("%d direct resumes succeeded, want at most one", resumed)
	}
	if runner.count("deploy") != 1 {
		t.Errorf("deploy ran %d times across the replicas, want exactly once", runner.count("deploy"))
	}
	steps, err := store.Steps(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Steps() error = %v", err)
	}
	deploys := 0
	for _, s := range steps {
		if s.StepName == "deploy" {
			deploys++
		}
	}
	if deploys != 1 {
		t.Errorf("%d deploy step records, want one", deploys)
	}
}

// TestDecideStepRefusals pins who may decide an approval step and what the decision binds to.
//
//nolint:funlen // Test function.
func TestDecideStepRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Distinct requires a different approver than the launcher.
		Distinct bool
		// Decision is the decision made.
		Decision StepDecision
		// Twice decides the step once before the decision under test.
		Twice bool
		// Target is parent to aim the decision at the workflow rather than its step.
		Target string
		// Want is the error expected.
		Want error
	}{{ // Test 0: An agent can never approve.
		Decision: StepDecision{Approve: true, By: outcome.Decider{Name: "bot", Type: "agent"}},
		Want:     ErrAgentApproval,
	}, { // Test 1: The launcher cannot approve when the rule in force requires a second person.
		Distinct: true,
		Decision: StepDecision{Approve: true, By: outcome.Decider{Name: "requester", Type: "session"}},
		Want:     ErrSelfApproval,
	}, { // Test 2: The launcher may approve when no rule requires a second person, as a run allows.
		Decision: StepDecision{Approve: true, By: outcome.Decider{Name: "requester", Type: "session"}},
		Want:     nil,
	}, { // Test 3: The launcher may always deny, which withdraws their own request.
		Distinct: true,
		Decision: StepDecision{By: outcome.Decider{Name: "requester", Type: "session"}},
		Want:     nil,
	}, { // Test 4: A decision on a state the approver was not shown is refused.
		Decision: StepDecision{Approve: true, Shown: "sha256:stale",
			By: outcome.Decider{Name: "approver", Type: "session"}},
		Want: ErrStateMoved,
	}, { // Test 5: A step already decided cannot be decided again.
		Decision: StepDecision{Approve: true, By: outcome.Decider{Name: "approver", Type: "session"}},
		Twice:    true,
		Want:     ErrNotPendingApproval,
	}, { // Test 6: A step decision names an approval step, not the workflow.
		Decision: StepDecision{Approve: true, By: outcome.Decider{Name: "approver", Type: "session"}},
		Target:   "parent",
		Want:     ErrNotApprovalStep,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			audits := audit.NewMemStore()
			d := New(store, &commandRecorder{}, nil, WithNoJanitor(), WithAudits(audits))
			defer d.Close()
			parent, err := d.SubmitPipeline(ctx, "release", "", gatedWorkflow(0, true),
				run.WithActor("requester"), run.WithRequireDistinctApprover(test.Distinct))
			if err != nil {
				t.Fatalf("SubmitPipeline() error = %v", err)
			}
			node := waitParked(t, store, parent.ID)
			target := node.ID
			if test.Target == "parent" {
				target = parent.ID
			}
			before := len(stepPaths(t, audits))
			if test.Twice {
				if _, err := d.DecideStep(ctx, target, test.Decision); err != nil {
					t.Fatalf("first DecideStep() error = %v", err)
				}
				before = len(stepPaths(t, audits))
			}
			_, err = d.DecideStep(ctx, target, test.Decision)
			if !errors.Is(err, test.Want) {
				t.Fatalf("DecideStep() error = %v, want %v", err, test.Want)
			}
			if test.Want != nil {
				if after := len(stepPaths(t, audits)); after != before {
					t.Errorf("a refused decision left %d chain entries, want none", after-before)
				}
				if !test.Twice {
					got, gerr := store.Get(ctx, node.ID)
					if gerr != nil {
						t.Fatalf("Get() error = %v", gerr)
					}
					if got.Status != run.StatusPendingApproval {
						t.Errorf("refused decision moved the step to %q", got.Status)
					}
				}
			}
		})
	}
}

// TestApprovalStepBindsTheStateItWasGivenFor pins the binding. A decision recorded by a replica
// that died before resuming is resumed elsewhere, and a workflow whose finished step published
// something different since the approval is refused rather than run, the way an approved run whose
// spec changed is refused at execution.
func TestApprovalStepBindsTheStateItWasGivenFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Tamper rewrites an upstream step's output after the approval.
		Tamper bool
		// WantStatus is the workflow's final status.
		WantStatus run.Status
		// WantDeploy is how many times the approve path ran.
		WantDeploy int
	}{{ // Test 0: An untouched workflow resumes and runs the approved path.
		Tamper: false, WantStatus: run.StatusSucceeded, WantDeploy: 1,
	}, { // Test 1: A workflow whose upstream output changed after the approval is refused.
		Tamper: true, WantStatus: run.StatusFailed, WantDeploy: 0,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			runner := &commandRecorder{}
			d1 := New(store, runner, nil, WithNoJanitor(), WithOwner("replica-one"))
			parent, err := d1.SubmitPipeline(ctx, "release", "", gatedWorkflow(0, true))
			if err != nil {
				t.Fatalf("SubmitPipeline() error = %v", err)
			}
			node := waitParked(t, store, parent.ID)
			d1.Close()

			// The approval as DecideStep records it, minus the resume, which is what a replica that
			// died between the two leaves behind.
			stored, err := store.Get(ctx, parent.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			state, err := outcome.StepStateOf(ctx, store, stored, node)
			if err != nil {
				t.Fatalf("StepStateOf() error = %v", err)
			}
			digest, _ := state.Digest()
			binding, _ := state.Binding()
			if err := store.StampApprovedSpec(ctx, node.ID, digest, binding); err != nil {
				t.Fatalf("StampApprovedSpec() error = %v", err)
			}
			if ok, err := store.SettleHeld(ctx, node.ID, run.Finalization{
				Status: run.StatusSucceeded, EndedAt: time.Now()}); err != nil || !ok {
				t.Fatalf("SettleHeld() = (%v, %v)", ok, err)
			}
			if test.Tamper {
				steps, err := store.Steps(ctx, parent.ID)
				if err != nil {
					t.Fatalf("Steps() error = %v", err)
				}
				for _, s := range steps {
					if s.StepName == "build" {
						s.Outputs = map[string]any{"artifact": "somebody-elses-build"}
						if err := store.Save(ctx, s); err != nil {
							t.Fatalf("Save() error = %v", err)
						}
					}
				}
			}
			d2 := New(store, runner, nil, WithNoJanitor(), WithOwner("replica-two"))
			defer d2.Close()
			d2.sweepApprovalSteps()
			got := waitTerminal(t, store, parent.ID)
			if got.Status != test.WantStatus {
				t.Errorf("workflow status = %q (%s), want %q", got.Status, got.Error, test.WantStatus)
			}
			if runner.count("deploy") != test.WantDeploy || runner.count("notify") != 0 {
				t.Errorf("deploy ran %d and notify %d times, want %d and 0", runner.count("deploy"),
					runner.count("notify"), test.WantDeploy)
			}
			if test.Tamper && !strings.Contains(got.Error, "changed after approval step") {
				t.Errorf("refusal reason = %q, want it to say the workflow changed", got.Error)
			}
		})
	}
}

// TestApprovalStepRefusesAParentPinChangedAfterApproval proves the composition that lets a step's
// binding see a change to the parent workflow itself, not only to a sibling step's output.
// StepState.Binding folds in outcome.SpecBinding(parent), so the pinned-commit protection
// SpecBinding carries reaches every approval step a workflow pauses at, through the same binding
// TestApprovalStepBindsTheStateItWasGivenFor proves for a tampered step output. As in the top-level
// executor's case, the parent's PinnedCommit and CommitSHA are moved together, so checkPinnedCommit
// alone sees no contradiction and the step binding is what catches it.
func TestApprovalStepRefusesAParentPinChangedAfterApproval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	runner := &commandRecorder{}
	d1 := New(store, runner, nil, WithNoJanitor(), WithOwner("replica-one"))
	parent, err := d1.SubmitPipeline(ctx, "release", "", gatedWorkflow(0, true),
		run.WithPinnedCommit("aaa111"))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	node := waitParked(t, store, parent.ID)
	d1.Close()

	stored, err := store.Get(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	state, err := outcome.StepStateOf(ctx, store, stored, node)
	if err != nil {
		t.Fatalf("StepStateOf() error = %v", err)
	}
	digest, _ := state.Digest()
	binding, _ := state.Binding()
	if err := store.StampApprovedSpec(ctx, node.ID, digest, binding); err != nil {
		t.Fatalf("StampApprovedSpec() error = %v", err)
	}
	if ok, err := store.SettleHeld(ctx, node.ID, run.Finalization{
		Status: run.StatusSucceeded, EndedAt: time.Now()}); err != nil || !ok {
		t.Fatalf("SettleHeld() = (%v, %v)", ok, err)
	}

	// Whatever moved the parent's pin set CommitSHA to agree, the way an honest resync would.
	stored.PinnedCommit, stored.CommitSHA = "bbb222", "bbb222"
	if err := checkPinnedCommit(stored); err != nil {
		t.Fatalf("checkPinnedCommit() error = %v, want nil: the two fields agree, so this check "+
			"alone cannot see what this test is about", err)
	}
	if err := store.Save(ctx, stored); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	d2 := New(store, runner, nil, WithNoJanitor(), WithOwner("replica-two"))
	defer d2.Close()
	d2.sweepApprovalSteps()
	got := waitTerminal(t, store, parent.ID)
	if got.Status != run.StatusFailed {
		t.Fatalf("workflow status = %q (%s), want failed: the approval was for aaa111 and the "+
			"workflow is now pinned to bbb222", got.Status, got.Error)
	}
	if runner.count("deploy") != 0 || runner.count("notify") != 0 {
		t.Errorf("deploy ran %d and notify %d times, want 0 and 0", runner.count("deploy"),
			runner.count("notify"))
	}
	if !strings.Contains(got.Error, "changed after approval step") {
		t.Errorf("refusal reason = %q, want it to say the workflow changed", got.Error)
	}
}

// TestWholeRunDecisionRefusesAStartedWorkflow pins that a workflow paused at a step cannot be
// approved or rejected as a whole run. Approving it as a run would walk the graph from the top and
// run every finished step again.
func TestWholeRunDecisionRefusesAStartedWorkflow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Approve approves the workflow as a run, and false rejects it.
		Approve bool
		// Want is the error expected.
		Want error
	}{{ // Test 0: Approving the paused workflow as a run is refused.
		Approve: true, Want: ErrStepPending,
	}, { // Test 1: Rejecting the paused workflow as a run is refused.
		Approve: false, Want: ErrStepPending,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			runner := &commandRecorder{}
			d := New(store, runner, nil, WithNoJanitor())
			defer d.Close()
			parent, err := d.SubmitPipeline(ctx, "release", "", gatedWorkflow(0, true))
			if err != nil {
				t.Fatalf("SubmitPipeline() error = %v", err)
			}
			waitParked(t, store, parent.ID)
			by := outcome.Decider{Name: "approver", Type: "session"}
			if test.Approve {
				_, err = d.Approve(ctx, parent.ID, by)
			} else {
				_, err = d.Reject(ctx, parent.ID, "", by)
			}
			if !errors.Is(err, test.Want) {
				t.Fatalf("decision error = %v, want %v", err, test.Want)
			}
			time.Sleep(50 * time.Millisecond)
			if runner.count("build") != 1 {
				t.Errorf("build ran %d times, want once", runner.count("build"))
			}
		})
	}
}

// TestApprovalStepInheritsSeparationOfDuties pins that the separation-of-duties answer the rules in
// force give a workflow's steps reaches its approval steps, even when no rule holds the whole
// workflow. A plan-content rule demanding a second person does not hold a workflow at submission,
// and the approval step releasing the apply it covers must still need that second person.
func TestApprovalStepInheritsSeparationOfDuties(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	rules := policy.NewMemStore()
	if err := rules.Save(ctx, &policy.Policy{ID: "pol_plan", Name: "plan gate",
		Tool: run.ToolTerraform, MaxDestroy: 0, RequireDistinctApprover: true}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	d := New(store, &commandRecorder{}, nil, WithNoJanitor(), WithPolicies(rules))
	defer d.Close()
	steps := []run.PipelineStep{
		{Name: "gate", Type: run.StepApproval},
		{Name: "apply", Tool: run.ToolTerraform, Command: "infra", DependsOn: []string{"gate"}},
	}
	parent, err := d.SubmitPipeline(ctx, "infra", "", steps, run.WithActor("requester"))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	if parent.Status == run.StatusPendingApproval {
		t.Fatalf("the plan-content rule held the whole workflow, want it to run to the step")
	}
	node := waitParked(t, store, parent.ID)
	if !node.RequireDistinctApprover {
		t.Fatal("the approval step does not require a distinct approver")
	}
	_, err = d.DecideStep(ctx, node.ID, StepDecision{Approve: true,
		By: outcome.Decider{Name: "requester", Type: "session"}})
	if !errors.Is(err, ErrSelfApproval) {
		t.Errorf("self-approval error = %v, want ErrSelfApproval", err)
	}
}

// TestCancelingAParkedWorkflowWithdrawsItsStep pins that canceling a workflow paused at a step
// takes the step out of the approval queue. A step left waiting could be approved for a workflow
// that no longer exists.
func TestCancelingAParkedWorkflowWithdrawsItsStep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, &commandRecorder{}, nil, WithNoJanitor())
	defer d.Close()
	parent, err := d.SubmitPipeline(ctx, "release", "", gatedWorkflow(0, true))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	node := waitParked(t, store, parent.ID)
	if done, err := d.CancelWaiting(ctx, parent.ID); err != nil || !done {
		t.Fatalf("CancelWaiting() = (%v, %v), want (true, nil)", done, err)
	}
	got, err := store.Get(ctx, node.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusCanceled {
		t.Errorf("approval step status = %q, want canceled", got.Status)
	}
	if _, err := d.DecideStep(ctx, node.ID, StepDecision{Approve: true,
		By: outcome.Decider{Name: "approver", Type: "session"}}); !errors.Is(err, ErrNotPendingApproval) {
		t.Errorf("approving a withdrawn step error = %v, want ErrNotPendingApproval", err)
	}
}

// TestApprovalStepNotifiesItsOwnEvent pins that a workflow reaching an approval step tells the
// channels it is waiting through the path a held run takes, as its own event rather than as a held
// run: it names the step, what the step asks, and what each answer runs next.
func TestApprovalStepNotifiesItsOwnEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// step is the awaiting step a payload carries.
	type step struct {
		// Name is the step's name.
		Name string `json:"name"`
		// Description is what the step asks.
		Description string `json:"description"`
		// OnApprove names what an approval runs.
		OnApprove []string `json:"on_approve"`
		// OnDeny names what a denial runs.
		OnDeny []string `json:"on_deny"`
	}
	// event is the part of a webhook payload this test reads.
	type event struct {
		// Event is workflow.step_awaiting_approval for a step, run.held for a whole run.
		Event string `json:"event"`
		// Run is the run the event is about.
		Run struct {
			// ID is the run's id.
			ID string `json:"id"`
			// HeldByPolicy names what holds the run.
			HeldByPolicy string `json:"held_by_policy"`
			// AwaitingStep names the step the workflow waits at.
			AwaitingStep *step `json:"awaiting_step"`
		} `json:"run"`
	}
	events := make(chan event, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e event
		if err := json.NewDecoder(r.Body).Decode(&e); err == nil {
			events <- e
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	store := run.NewMemStore()
	d := New(store, &commandRecorder{}, nil, WithNoJanitor(), WithWebhooks([]string{srv.URL}),
		WithNotifyClient(http.DefaultClient))
	defer d.Close()
	parent, err := d.SubmitPipeline(ctx, "release", "", gatedWorkflow(0, true))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	waitParked(t, store, parent.ID)
	select {
	case got := <-events:
		want := event{Event: "workflow.step_awaiting_approval"}
		want.Run.ID = parent.ID
		want.Run.HeldByPolicy = `approval step "gate"`
		want.Run.AwaitingStep = &step{Name: "gate", Description: "Ship it?",
			OnApprove: []string{"deploy"}, OnDeny: []string{"notify"}}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("notification mismatch (-want +got):\n%s", diff)
		}
	case <-time.After(waitBudget):
		t.Fatal("no notification arrived for the waiting approval step")
	}
}

// TestApprovalStepNotifiesNamedTargets pins that a workflow waiting at an approval step reaches
// the named targets attached for the approval event, the way a held run does, so an operator who
// routes approvals through a named target hears about a step hold as well as a whole-run hold. The
// router is asked about the workflow, held, and names the step, and what it returns is delivered.
func TestApprovalStepNotifiesNamedTargets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// asked is what the router was asked about.
	type asked struct {
		// ID is the run the router was asked about.
		ID string
		// Status is that run's status when asked.
		Status run.Status
		// HeldByPolicy is what that run said holds it.
		HeldByPolicy string
	}
	seen := make(chan asked, 8)
	delivered := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e struct {
			// Event is run.held or run.finished.
			Event string `json:"event"`
		}
		if err := json.NewDecoder(r.Body).Decode(&e); err == nil {
			delivered <- e.Event
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	router := NotificationRouterFunc(func(_ context.Context, r *run.Run) []run.NotifyTarget {
		seen <- asked{ID: r.ID, Status: r.Status, HeldByPolicy: r.HeldByPolicy}
		if r.Status != run.StatusPendingApproval {
			return nil
		}
		return []run.NotifyTarget{{Kind: run.NotifyWebhook, URL: srv.URL}}
	})
	store := run.NewMemStore()
	d := New(store, &commandRecorder{}, nil, WithNoJanitor(), WithNotificationRouter(router),
		WithNotifyClient(http.DefaultClient))
	defer d.Close()
	parent, err := d.SubmitPipeline(ctx, "release", "", gatedWorkflow(0, true))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	waitParked(t, store, parent.ID)
	want := asked{ID: parent.ID, Status: run.StatusPendingApproval,
		HeldByPolicy: `approval step "gate"`}
	deadline := time.After(waitBudget)
	for found := false; !found; {
		select {
		case got := <-seen:
			found = got == want
		case <-deadline:
			t.Fatal("the router was never asked about the workflow waiting at its approval step")
		}
	}
	select {
	case got := <-delivered:
		if diff := cmp.Diff("workflow.step_awaiting_approval", got); diff != "" {
			t.Errorf("delivered event mismatch (-want +got):\n%s", diff)
		}
	case <-time.After(waitBudget):
		t.Fatal("the named target returned for the approval event received nothing")
	}
}

// TestValidateApprovalSteps pins what an approval step may and may not carry.
func TestValidateApprovalSteps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Steps is the pipeline validated.
		Steps []run.PipelineStep
		// Want is the error expected.
		Want error
	}{{ // Test 0: A gated workflow with a deny path is valid.
		Steps: gatedWorkflow(60, true), Want: nil,
	}, { // Test 1: An approval step needs a name.
		Steps: []run.PipelineStep{{Type: run.StepApproval}}, Want: run.ErrApprovalStep,
	}, { // Test 2: An approval step runs nothing, so it takes no command.
		Steps: []run.PipelineStep{{Name: "gate", Type: run.StepApproval, Command: "x"}},
		Want:  run.ErrApprovalStep,
	}, { // Test 3: Continue on failure would run the approve path after a denial.
		Steps: []run.PipelineStep{{Name: "gate", Type: run.StepApproval, ContinueOnFailure: true}},
		Want:  run.ErrApprovalStep,
	}, { // Test 4: A deny path names an approval step.
		Steps: []run.PipelineStep{
			{Name: "build", Tool: run.ToolBash, Command: "b"},
			{Name: "after", Tool: run.ToolBash, Command: "a", IfDenied: []string{"build"}},
		},
		Want: run.ErrDenyPath,
	}, { // Test 5: Waiting for an approval and handling its denial can never both hold.
		Steps: []run.PipelineStep{
			{Name: "gate", Type: run.StepApproval},
			{Name: "after", Tool: run.ToolBash, Command: "a", DependsOn: []string{"gate"},
				IfDenied: []string{"gate"}},
		},
		Want: run.ErrDenyPath,
	}, { // Test 6: An unknown step type is refused rather than run as a tool step.
		Steps: []run.PipelineStep{{Name: "x", Type: "pause", Tool: run.ToolBash, Command: "a"}},
		Want:  run.ErrApprovalStep,
	}, { // Test 7: A negative timeout is refused.
		Steps: []run.PipelineStep{{Name: "gate", Type: run.StepApproval, ApprovalTimeout: -1}},
		Want:  run.ErrApprovalStep,
	}, { // Test 8: A sequence holding an approval step must name every step.
		Steps: []run.PipelineStep{
			{Tool: run.ToolBash, Command: "a"}, {Name: "gate", Type: run.StepApproval},
		},
		Want: run.ErrUnnamedStep,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if err := run.ValidatePipeline(test.Steps); !errors.Is(err, test.Want) {
				t.Errorf("ValidatePipeline() error = %v, want %v", err, test.Want)
			}
		})
	}
}

// TestApprovalStepStateNamesBothPaths pins what an approver is shown: the steps the approval waited
// behind and the steps each answer runs.
func TestApprovalStepStateNamesBothPaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, &commandRecorder{}, nil, WithNoJanitor())
	defer d.Close()
	parent, err := d.SubmitPipeline(ctx, "release", "", gatedWorkflow(0, true))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	node := waitParked(t, store, parent.ID)
	stored, err := store.Get(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	state, err := outcome.StepStateOf(ctx, store, stored, node)
	if err != nil {
		t.Fatalf("StepStateOf() error = %v", err)
	}
	upstream := make([]string, 0, len(state.Upstream))
	for _, u := range state.Upstream {
		upstream = append(upstream, u.Name+":"+u.Status)
	}
	got := map[string][]string{"upstream": upstream, "approve": state.OnApprove, "deny": state.OnDeny}
	want := map[string][]string{"upstream": {"build:succeeded"}, "approve": {"deploy"},
		"deny": {"notify"}}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("state mismatch (-want +got):\n%s", diff)
	}
}
