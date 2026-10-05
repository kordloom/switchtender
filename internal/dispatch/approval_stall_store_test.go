package dispatch_test

import (
	"context"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// windowRunner succeeds and counts each command it executes, shared by every replica so the count
// is the whole cluster's.
type windowRunner struct {
	// mu guards seen.
	mu sync.Mutex
	// seen counts executions per command.
	seen map[string]int
}

// Run counts the command and succeeds.
func (c *windowRunner) Run(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen[spec.Command]++
	return roundhouse.Result{ExitCode: 0}, nil
}

// count returns how many times command executed.
func (c *windowRunner) count(command string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen[command]
}

// windowChain returns the verdict of every approval step entry on the chain, in order, and how
// many outcome entries it holds for the run id.
func windowChain(t *testing.T, audits audit.Store, id string) ([]string, int) {
	t.Helper()
	chain, err := audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	var verdicts []string
	outcomes := 0
	for _, e := range chain {
		if _, _, verdict, ok := outcome.ParseStepDecisionPath(e.Path); ok {
			verdicts = append(verdicts, verdict)
		}
		if strings.HasPrefix(e.Path, "/runs/"+id+"/outcome/") {
			outcomes++
		}
	}
	return verdicts, outcomes
}

// TestSweepParksAWorkflowStalledAtItsApprovalStep holds a workflow's coordinator after its approval
// step is listed and before its park, the way a process that dies there leaves it, and approves the
// listed step. The approval is accepted while the workflow stays running under a lease nobody
// renews. A second replica's lease sweep must then park the workflow and resume it on that
// approval, running the approve path exactly once with one request, one decision, and one outcome
// on the chain. Interrupting it lost the approval. The held coordinator, let go afterward, must
// leave the finished workflow alone.
func TestSweepParksAWorkflowStalledAtItsApprovalStep(t *testing.T) {
	t.Parallel()
	steps := []run.PipelineStep{
		{Name: "build", Tool: run.ToolBash, Command: "build"},
		{Name: "gate", Type: run.StepApproval, Description: "Ship it?", DependsOn: []string{"build"}},
		{Name: "deploy", Tool: run.ToolBash, Command: "deploy", DependsOn: []string{"gate"}},
		{Name: "notify", Tool: run.ToolBash, Command: "notify", IfDenied: []string{"gate"}},
	}
	for _, backend := range leaseBackends() {
		t.Run(backend.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := backend.Open(t)
			audits := audit.NewMemStore()
			runner := &windowRunner{seen: map[string]int{}}
			reached, release := make(chan string, 1), make(chan struct{})
			var once sync.Once
			free := func() { once.Do(func() { close(release) }) }
			first := dispatch.New(store, runner, nil, dispatch.WithNoJanitor(),
				dispatch.WithAudits(audits), dispatch.WithOwner("first"),
				dispatch.WithParkHook(func(id string) {
					reached <- id
					<-release
				}))
			t.Cleanup(first.Close)
			t.Cleanup(free)
			parent, err := first.SubmitPipeline(ctx, "release", "", steps)
			if err != nil {
				t.Fatalf("SubmitPipeline() error = %v", err)
			}
			select {
			case <-reached:
			case <-time.After(60 * time.Second):
				t.Fatal("the workflow never reached its park")
			}
			pending, err := run.PendingApprovalSteps(ctx, store, parent.ID)
			if err != nil || len(pending) != 1 {
				t.Fatalf("PendingApprovalSteps() = %d, %v, want the step listed", len(pending), err)
			}
			if _, err := first.DecideStep(ctx, pending[0].ID, dispatch.StepDecision{Approve: true,
				By: outcome.Decider{Name: "approver", Type: "session"}}); err != nil {
				t.Fatalf("DecideStep() error = %v, want the listed step's approval accepted", err)
			}
			// The coordinator never parks, so its lease ages as a dead process's would.
			held, err := store.Get(ctx, parent.ID)
			if err != nil || held.Status != run.StatusRunning || held.ClaimedBy != "first" {
				t.Fatalf("the held workflow = %+v, %v, want running under its coordinator", held, err)
			}
			old := time.Now().Add(-time.Hour)
			held.ClaimedAt = &old
			if err := store.Save(ctx, held); err != nil {
				t.Fatalf("Save() error = %v", err)
			}

			second := dispatch.New(store, runner, nil, dispatch.WithAudits(audits),
				dispatch.WithOwner("second"), dispatch.WithClaimInterval(20*time.Millisecond))
			t.Cleanup(second.Close)
			if got := schWaitDone(t, store, parent.ID); got.Status != run.StatusSucceeded {
				t.Errorf("workflow = %q (%s), want succeeded on the approval", got.Status, got.Error)
			}

			free()
			first.Close()
			second.Close()
			if got, err := store.Get(ctx, parent.ID); err != nil || got.Status != run.StatusSucceeded {
				t.Errorf("after the held coordinator went on, the workflow = %v, %v, want succeeded",
					got.Status, err)
			}
			if runner.count("build") != 1 || runner.count("deploy") != 1 || runner.count("notify") != 0 {
				t.Errorf("build, deploy, notify ran %d, %d, %d times, want 1, 1, 0",
					runner.count("build"), runner.count("deploy"), runner.count("notify"))
			}
			verdicts, outcomes := windowChain(t, audits, parent.ID)
			if !slices.Equal(verdicts, []string{outcome.StepRequested, outcome.StepApproved}) ||
				outcomes != 1 {
				t.Errorf("chain holds step entries %v and %d workflow outcomes, want one request, "+
					"one approval, and one outcome", verdicts, outcomes)
			}
		})
	}
}
