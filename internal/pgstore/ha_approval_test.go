package pgstore_test

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// commandCounter is a runner that succeeds and counts each command it executes, shared by every
// replica so the count is the whole cluster's.
type commandCounter struct {
	// mu guards seen.
	mu sync.Mutex
	// seen counts executions per command.
	seen map[string]int
}

// Run counts the command and succeeds.
func (c *commandCounter) Run(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen[spec.Command]++
	return roundhouse.Result{ExitCode: 0}, nil
}

// count returns how many times command executed.
func (c *commandCounter) count(command string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen[command]
}

// TestHAApprovalStepSurvivesARestartAndResumesOnce proves a workflow paused at an approval step on
// PostgreSQL survives the replica that paused it, and that when several replicas see the decision
// at once exactly one of them continues it. The replica that parked the workflow stops. The
// decision is recorded the way a replica that died before resuming leaves it. Three fresh replicas
// then start together, and every one of their janitors races to resume the same workflow.
func TestHAApprovalStepSurvivesARestartAndResumesOnce(t *testing.T) {
	dsn := testDSN(t)
	openReplica(t, dsn)
	truncateAll(t, dsn)
	ctx := context.Background()
	runner := &commandCounter{seen: map[string]int{}}
	store := openReplica(t, dsn).Runs()

	first := dispatch.New(openReplica(t, dsn).Runs(), runner, zap.NewNop(),
		dispatch.WithOwner("replica-first"), dispatch.WithClaimInterval(20*time.Millisecond))
	parent, err := first.SubmitPipeline(ctx, "release", "", []run.PipelineStep{
		{Name: "build", Tool: run.ToolBash, Command: "build"},
		{Name: "gate", Type: run.StepApproval, Description: "Ship it?", DependsOn: []string{"build"}},
		{Name: "deploy", Tool: run.ToolBash, Command: "deploy", DependsOn: []string{"gate"}},
		{Name: "notify", Tool: run.ToolBash, Command: "notify", IfDenied: []string{"gate"}},
	})
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	var node *run.Run
	waitFor(t, "the workflow to park at its approval step", func() bool {
		p, err := store.Get(ctx, parent.ID)
		if err != nil || p.Status != run.StatusPendingApproval || p.ClaimedBy != "" {
			return false
		}
		pending, err := run.PendingApprovalSteps(ctx, store, parent.ID)
		if err != nil || len(pending) != 1 {
			return false
		}
		node = pending[0]
		return true
	})
	first.Close()

	stored, err := store.Get(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	state, err := outcome.StepStateOf(ctx, store, stored, node)
	if err != nil {
		t.Fatalf("StepStateOf() error = %v", err)
	}
	digest, err := state.Digest()
	if err != nil {
		t.Fatalf("Digest() error = %v", err)
	}
	binding, err := state.Binding()
	if err != nil {
		t.Fatalf("Binding() error = %v", err)
	}
	if err := store.StampApprovedSpec(ctx, node.ID, digest, binding); err != nil {
		t.Fatalf("StampApprovedSpec() error = %v", err)
	}
	if ok, err := store.SettleHeld(ctx, node.ID, run.Finalization{Status: run.StatusSucceeded,
		EndedAt: time.Now()}); err != nil || !ok {
		t.Fatalf("SettleHeld() = (%v, %v), want (true, nil)", ok, err)
	}

	var wg sync.WaitGroup
	replicas := make([]*dispatch.Dispatcher, 3)
	for i := range replicas {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			replicas[i] = dispatch.New(openReplica(t, dsn).Runs(), runner, zap.NewNop(),
				dispatch.WithOwner(fmt.Sprintf("replica-%d", i)),
				dispatch.WithClaimInterval(20*time.Millisecond))
		}(i)
	}
	wg.Wait()
	defer func() {
		for _, d := range replicas {
			d.Close()
		}
	}()

	waitFor(t, "the resumed workflow to finish", func() bool {
		p, err := store.Get(ctx, parent.ID)
		return err == nil && p.Status.Terminal()
	})
	got, err := store.Get(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusSucceeded {
		t.Errorf("workflow status = %q (%s), want succeeded", got.Status, got.Error)
	}
	if runner.count("build") != 1 || runner.count("deploy") != 1 || runner.count("notify") != 0 {
		t.Errorf("build, deploy, notify ran %d, %d, %d times, want 1, 1, 0", runner.count("build"),
			runner.count("deploy"), runner.count("notify"))
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
		t.Errorf("%d deploy step records across the replicas, want one", deploys)
	}
}
