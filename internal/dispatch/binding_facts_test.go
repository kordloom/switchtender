package dispatch

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/plantest"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// phaseRunner counts the plans and the applies it is handed.
type phaseRunner struct {
	// mu guards the counts.
	mu sync.Mutex
	// plans counts dry runs.
	plans int
	// applies counts real runs.
	applies int
}

// Run counts the phase and succeeds, a plan reporting nothing to destroy.
func (p *phaseRunner) Run(_ context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result,
	error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if spec.DryRun {
		p.plans++
		_, _ = io.WriteString(out, "Plan: 0 to add, 0 to change, 0 to destroy\n")
		return plantest.Result("Plan: 0 to add, 0 to change, 0 to destroy"), nil
	}
	p.applies++
	_, _ = io.WriteString(out, "Apply complete!\n")
	return roundhouse.Result{ExitCode: 0}, nil
}

// TestARowWriterCannotSkipThePlanFirst pins the fields a rule and the plan gate read to the seal. A
// terraform apply that asked for approval, and one an agent asked for, plan first: the plan runs
// and the apply waits for a person. A database writer who marks the row approved by stamping its
// spec digest, names another run's decision, or turns the agent's approved run into a person's
// would have the executor apply it straight away. Each is refused with nothing run, and the honest
// twin of each plans first.
func TestARowWriterCannotSkipThePlanFirst(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Edit returns the writer's change to the row, nil for the honest twin.
		Edit func(t *testing.T, r, decided *run.Run) func(*run.Run)
		// WantError is what the run's error must say, empty for a run that succeeds.
		WantError string
		// WantPlans is how many plans must run.
		WantPlans int
		// Agent submits the run as an agent and approves it, rather than asking for approval.
		Agent bool
	}{{ // Test 0: An apply that asked for approval plans first.
		WantPlans: 1,
	}, { // Test 1: Its spec digest stamped by a writer is refused.
		Edit: func(t *testing.T, r, _ *run.Run) func(*run.Run) {
			t.Helper()
			digest, err := outcome.SpecDigest(r)
			if err != nil {
				t.Fatalf("SpecDigest() error = %v", err)
			}
			return func(row *run.Run) { row.ApprovedSpecDigest = digest }
		},
		WantError: "changed after it was submitted",
	}, { // Test 2: Another run's decision named on it is refused.
		Edit: func(_ *testing.T, _, decided *run.Run) func(*run.Run) {
			return func(row *run.Run) { row.DecisionID = decided.DecisionID }
		},
		WantError: "is not the decision this row names",
	}, { // Test 3: An agent's approved apply plans first.
		Agent: true, WantPlans: 1,
	}, { // Test 4: The agent turned into a person after its approval is refused.
		Agent: true,
		Edit: func(_ *testing.T, _, _ *run.Run) func(*run.Run) {
			return func(row *run.Run) { row.ActorType, row.Initiator = "session", nil }
		},
		WantError: "changed after it was approved",
	}, { // Test 5: The agent's initiator dropped alone after its approval is refused.
		Agent: true,
		Edit: func(_ *testing.T, _, _ *run.Run) func(*run.Run) {
			return func(row *run.Run) {
				row.Initiator = &run.Initiator{InitiatedBy: "someone-else"}
			}
		},
		WantError: "changed after it was approved",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			key := bndKey(t)
			store, chain := newBndRows(), &bndChain{Store: audit.NewMemStore()}
			submitter := New(store, dispatchtest.OKRunner(), nil, WithNoJanitor(), WithAudits(chain),
				WithBindingKey(key), WithQueues([]string{"decisions"}))
			opts := []run.SubmitOption{run.WithTool(run.ToolTerraform),
				run.WithCommand("infra/prod"), run.WithQueue("work")}
			if test.Agent {
				opts = append(opts, run.WithActor("agent-1"), run.WithActorType("agent"))
			} else {
				opts = append(opts, run.WithRequireApproval(true))
			}
			r, err := submitter.Submit(ctx, "", "", opts...)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if test.Agent {
				if r, err = submitter.Approve(ctx, r.ID, bndDecider()); err != nil {
					t.Fatalf("Approve() error = %v", err)
				}
			}
			held, err := submitter.Submit(ctx, "", "", run.WithTool(run.ToolBash),
				run.WithCommand("other"), run.WithQueue("parked"), run.WithRequireApproval(true))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			decided, err := submitter.Approve(ctx, held.ID, bndDecider())
			if err != nil {
				t.Fatalf("Approve() error = %v", err)
			}
			submitter.Close()
			if test.Edit != nil {
				store.rewrite(r.ID, test.Edit(t, r, decided))
			}
			runner := &phaseRunner{}
			executor := New(store, runner, nil, WithNoJanitor(), WithAudits(chain),
				WithBindingKey(key), WithQueues([]string{"work"}),
				WithClaimInterval(5*time.Millisecond))
			defer executor.Close()
			got := dispatchtest.WaitTerminal(t, store.Store, r.ID)
			executor.Close()
			runner.mu.Lock()
			defer runner.mu.Unlock()
			if runner.applies != 0 {
				t.Errorf("the executor applied %d times, want 0: an apply must wait for a person "+
					"after its plan", runner.applies)
			}
			if runner.plans != test.WantPlans {
				t.Errorf("the executor planned %d times, want %d", runner.plans, test.WantPlans)
			}
			if test.WantError == "" {
				if got.Status != run.StatusSucceeded {
					t.Errorf("the honest run ended %s (%s), want succeeded", got.Status, got.Error)
				}
				return
			}
			if got.Status != run.StatusFailed || !strings.Contains(got.Error, "refused") ||
				!strings.Contains(got.Error, test.WantError) {
				t.Errorf("the rewritten run ended %s (%s), want failed and refused with %q",
					got.Status, got.Error, test.WantError)
			}
		})
	}
}

// TestAForgedTimeoutIsNotTimedOut pins the approval sweep to the seals. The sweep reads a waiting
// step's timeout and creation time from its row and takes the deny path once the time has passed,
// so a database writer who backdates the step, or gives a step with no timeout one, would have the
// deny path run on terms nobody set. Each is refused: the step is not timed out, neither path runs,
// and the workflow fails with the reason. An honest timeout still takes the deny path.
func TestAForgedTimeoutIsNotTimedOut(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Edit is the writer's change to the waiting step's row, nil for an honest one.
		Edit func(r *run.Run)
		// WantWorkflow is how the workflow must stand after the sweep.
		WantWorkflow run.Status
		// WantError is what the workflow's error must say, empty when it has none.
		WantError string
		// WantNotify is how many times the deny path must run.
		WantNotify int
		// Wait is how long the step waits before the sweep, real time on an honest row.
		Wait time.Duration
		// Timeout is the step's approval timeout in seconds.
		Timeout int
	}{{ // Test 0: An hour's timeout nobody reached leaves the step waiting.
		Timeout: 3600, WantWorkflow: run.StatusPendingApproval,
	}, { // Test 1: A second's timeout that passed takes the deny path.
		Timeout: 1, Wait: 1100 * time.Millisecond, WantWorkflow: run.StatusSucceeded,
		WantNotify: 1,
	}, { // Test 2: The step backdated past its hour is refused.
		Timeout: 3600, WantWorkflow: run.StatusFailed,
		Edit:      func(r *run.Run) { r.CreatedAt = r.CreatedAt.Add(-2 * time.Hour) },
		WantError: "changed after it was built",
	}, { // Test 3: A timeout given to a step built with none is refused.
		Timeout: 0, WantWorkflow: run.StatusFailed,
		Edit:      func(r *run.Run) { r.Timeout, r.CreatedAt = 1, r.CreatedAt.Add(-time.Hour) },
		WantError: "changed after it was built",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store, chain := run.NewMemStore(), audit.NewMemStore()
			runner := &commandRecorder{}
			d := New(store, runner, nil, WithNoJanitor(), WithAudits(chain), WithBindingKey(bndKey(t)))
			defer d.Close()
			parent, err := d.SubmitPipeline(ctx, "release", "", gatedWorkflow(test.Timeout, true))
			if err != nil {
				t.Fatalf("SubmitPipeline() error = %v", err)
			}
			node := waitParked(t, store, parent.ID)
			if test.Edit != nil {
				row, err := store.Get(ctx, node.ID)
				if err != nil {
					t.Fatalf("Get() error = %v", err)
				}
				test.Edit(row)
				if err := store.Save(ctx, row); err != nil {
					t.Fatalf("Save() error = %v", err)
				}
			}
			time.Sleep(test.Wait)
			if err := sweepSteps(d, node); err != nil {
				t.Fatal(err)
			}
			got := parent
			if test.WantWorkflow == run.StatusPendingApproval {
				if got, err = store.Get(ctx, parent.ID); err != nil {
					t.Fatalf("Get() error = %v", err)
				}
			} else {
				got = dispatchtest.WaitTerminal(t, store, parent.ID)
			}
			if got.Status != test.WantWorkflow || !strings.Contains(got.Error, test.WantError) {
				t.Errorf("the workflow ended %s (%s), want %s with %q", got.Status, got.Error,
					test.WantWorkflow, test.WantError)
			}
			if test.WantError != "" && !strings.Contains(got.Error, "refused") {
				t.Errorf("the workflow's error %q does not say it was refused", got.Error)
			}
			if n := runner.count("notify"); n != test.WantNotify {
				t.Errorf("the deny path ran %d times, want %d", n, test.WantNotify)
			}
			if n := runner.count("deploy"); n != 0 {
				t.Errorf("the approve path ran %d times, want 0: nobody approved the step", n)
			}
			if test.Edit == nil {
				return
			}
			step, err := store.Get(ctx, node.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if strings.Contains(step.Error, "timed out") {
				t.Errorf("the rewritten step reads %q, want it never timed out", step.Error)
			}
		})
	}
}
