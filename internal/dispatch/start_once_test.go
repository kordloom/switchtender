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
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// startChain is an audit store whose START appends can fail the way an unreachable database's do,
// and can land while their answer is lost. Every other append and every read passes through.
type startChain struct {
	audit.Store
	// mu guards fails.
	mu sync.Mutex
	// fails is how many START appends fail before appends succeed again. A negative count fails
	// every one.
	fails int
	// land makes a failing START append land on the chain before it answers with the failure.
	land bool
}

// Append fails a START entry while fails says so, and appends everything else.
func (c *startChain) Append(ctx context.Context, e *audit.Entry) error {
	if e.Method != audit.MethodStart {
		return c.Store.Append(ctx, e)
	}
	c.mu.Lock()
	fail, land := c.fails != 0, c.land
	if c.fails > 0 {
		c.fails--
	}
	c.mu.Unlock()
	if !fail {
		return c.Store.Append(ctx, e)
	}
	if land {
		if err := c.Store.Append(ctx, e); err != nil {
			return err
		}
	}
	return errBndUnreachable
}

// failFirstRunner fails the first failures executions of a command and succeeds after that,
// counting every execution.
type failFirstRunner struct {
	// mu guards seen.
	mu sync.Mutex
	// seen counts executions per command.
	seen map[string]int
	// failures is how many executions of each command fail before one succeeds.
	failures int
}

// Run counts the command and fails while its failures last.
func (f *failFirstRunner) Run(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result,
	error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seen == nil {
		f.seen = map[string]int{}
	}
	f.seen[spec.Command]++
	if f.seen[spec.Command] <= f.failures {
		return roundhouse.Result{ExitCode: 2}, nil
	}
	return roundhouse.Result{ExitCode: 0}, nil
}

// count returns how many times command executed.
func (f *failFirstRunner) count(command string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[command]
}

// runEntries returns the chain's entries for run id whose method is method, oldest first.
func runEntries(t *testing.T, chain audit.Store, id, method string) []*audit.Entry {
	t.Helper()
	all, err := chain.List(context.Background(), 1<<20)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var out []*audit.Entry
	for i := len(all) - 1; i >= 0; i-- {
		if e := all[i]; e.Method == method && strings.HasPrefix(e.Path, "/runs/"+id+"/") {
			out = append(out, e)
		}
	}
	return out
}

// waitOutcome waits until the chain holds an outcome entry for run id. The outcome is committed
// after the run's terminal write, so a run read as finished may not have one yet.
func waitOutcome(t *testing.T, chain audit.Store, id string) {
	t.Helper()
	deadline := time.Now().Add(dispatchtest.WaitBudget)
	for len(runEntries(t, chain, id, audit.MethodRun)) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("run %s never committed its outcome", id)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// resetToPending does what a principal who can write the runs table does to replay a finished
// run: the row goes back to pending with its claim, its times, and its result cleared, and every
// field a seal covers left as it was.
func resetToPending(t *testing.T, store run.Store, id string) {
	t.Helper()
	ctx := context.Background()
	r, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	r.Status, r.ClaimedBy, r.ClaimSecret, r.Error = run.StatusPending, "", "", ""
	r.ClaimedAt, r.StartedAt, r.EndedAt, r.ExitCode = nil, nil, nil, nil
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
}

// TestAResetFinishedApprovedRunIsRefusedAndRunsNothing pins that an attempt starts once. A run's
// status is in no seal, so a database writer can reset a finished approved run to pending and the
// executor would run the approved change again under a valid seal. The start of each attempt is
// fixed on the chain under an id the run and its attempt decide, so the replay is refused and runs
// nothing, and the chain holds one start and one outcome for the run.
func TestAResetFinishedApprovedRunIsRefusedAndRunsNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// WantStatus is how the run must stand at the end.
		WantStatus run.Status
		// WantError is what the run's error must say after the reset, empty for no reset.
		WantError string
		// Reset replays the finished run.
		Reset bool
	}{{ // Test 0: The approved run runs once and records one start.
		WantStatus: run.StatusSucceeded,
	}, { // Test 1: The run reset after it finished is refused and runs nothing.
		Reset: true, WantStatus: run.StatusFailed, WantError: "already started once",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store, chain := run.NewMemStore(), audit.NewMemStore()
			runner := &failFirstRunner{}
			d := New(store, runner, nil, WithNoJanitor(), WithAudits(chain), WithBindingKey(bndKey(t)),
				WithClaimInterval(5*time.Millisecond))
			defer d.Close()
			held, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("deploy"),
				run.WithRequireApproval(true))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if _, err := d.Approve(ctx, held.ID, bndDecider()); err != nil {
				t.Fatalf("Approve() error = %v", err)
			}
			got := dispatchtest.WaitTerminal(t, store, held.ID)
			if got.Status != run.StatusSucceeded {
				t.Fatalf("the approved run ended %s (%s), want succeeded", got.Status, got.Error)
			}
			waitOutcome(t, chain, held.ID)
			if test.Reset {
				resetToPending(t, store, held.ID)
				got = dispatchtest.WaitTerminal(t, store, held.ID)
			}
			if n := runner.count("deploy"); n != 1 {
				t.Errorf("deploy ran %d times, want 1", n)
			}
			if got.Status != test.WantStatus {
				t.Errorf("the run ended %s (%s), want %s", got.Status, got.Error, test.WantStatus)
			}
			if test.WantError != "" && (!strings.HasPrefix(got.Error, "refused") ||
				!strings.Contains(got.Error, test.WantError)) {
				t.Errorf("the replayed run's error = %q, want it refused with %q", got.Error,
					test.WantError)
			}
			starts := runEntries(t, chain, held.ID, audit.MethodStart)
			if len(starts) != 1 || starts[0].ID != audit.StartID(held.ID, 0) ||
				starts[0].Path != audit.StartPath(held.ID, 0) {
				t.Errorf("chain holds %d starts for the run, want 1 under the fixed id", len(starts))
			}
			if n := len(runEntries(t, chain, held.ID, audit.MethodRun)); n != 1 {
				t.Errorf("chain holds %d outcomes for the run, want 1", n)
			}
		})
	}
}

// TestAnHonestRetryGetsANewAttemptAndRuns pins that the start entry does not stop a retry. A
// workflow step that fails and retries is a new record at the next attempt, so its start is a
// different entry and the retry runs.
func TestAnHonestRetryGetsANewAttemptAndRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, chain := run.NewMemStore(), audit.NewMemStore()
	runner := &failFirstRunner{failures: 1}
	d := New(store, runner, nil, WithNoJanitor(), WithAudits(chain), WithBindingKey(bndKey(t)),
		WithClaimInterval(5*time.Millisecond))
	defer d.Close()
	parent, err := d.SubmitPipeline(ctx, "release", "", []run.PipelineStep{
		{Name: "deploy", Tool: run.ToolBash, Command: "deploy", Retries: 1}})
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	if got := dispatchtest.WaitTerminal(t, store, parent.ID); got.Status != run.StatusSucceeded {
		t.Fatalf("workflow ended %s (%s), want succeeded on the retry", got.Status, got.Error)
	}
	if n := runner.count("deploy"); n != 2 {
		t.Errorf("deploy ran %d times, want 2", n)
	}
	steps, err := store.Steps(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Steps() error = %v", err)
	}
	ids := map[string]bool{}
	for _, s := range steps {
		starts := runEntries(t, chain, s.ID, audit.MethodStart)
		if len(starts) != 1 || starts[0].ID != audit.StartID(s.ID, s.Attempt) {
			t.Errorf("attempt %d holds %d starts, want 1 under its own id", s.Attempt, len(starts))
			continue
		}
		ids[starts[0].ID] = true
	}
	if len(steps) != 2 || len(ids) != 2 {
		t.Errorf("%d attempts with %d distinct starts, want 2 and 2", len(steps), len(ids))
	}
	if n := len(runEntries(t, chain, parent.ID, audit.MethodStart)); n != 0 {
		t.Errorf("the workflow itself holds %d starts, want 0: a coordinator runs no tool", n)
	}
}

// TestAStartTheChainCouldNotRecordRunsNothing pins that a start fails closed. A start the chain did
// not take runs nothing and says it could not be verified, never that it was refused, since nothing
// was tampered with. An append that landed with its answer lost cannot be told from an earlier
// start by any read, so it fails closed the same way. A chain that recovers inside the retries
// takes the start and the run runs once.
func TestAStartTheChainCouldNotRecordRunsNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// WantStatus is how the run must end.
		WantStatus run.Status
		// WantRuns is how many times the run's tool must run.
		WantRuns int
		// WantStarts is how many START entries the chain must hold for the run.
		WantStarts int
		// Fails is how many START appends fail, negative for every one.
		Fails int
		// Land makes a failing append land with its answer lost.
		Land bool
	}{{ // Test 0: A chain that takes no start runs nothing.
		Fails: -1, WantStatus: run.StatusFailed,
	}, { // Test 1: A start that landed with its answer lost fails closed.
		Fails: 1, Land: true, WantStatus: run.StatusFailed, WantStarts: 1,
	}, { // Test 2: A chain that recovers on the second try takes the start, and the run runs once.
		Fails: 1, WantStatus: run.StatusSucceeded, WantRuns: 1, WantStarts: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			chain := &startChain{Store: audit.NewMemStore(), fails: test.Fails, land: test.Land}
			runner := &failFirstRunner{}
			d := New(store, runner, nil, WithNoJanitor(), WithAudits(chain), WithBindingKey(bndKey(t)),
				WithClaimInterval(5*time.Millisecond))
			defer d.Close()
			r, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("deploy"))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			got := dispatchtest.WaitTerminal(t, store, r.ID)
			if got.Status != test.WantStatus {
				t.Errorf("run ended %s (%s), want %s", got.Status, got.Error, test.WantStatus)
			}
			if test.WantStatus == run.StatusFailed && (!strings.HasPrefix(got.Error, "could not verify") ||
				strings.Contains(got.Error, "refused")) {
				t.Errorf("run error = %q, want it to say could not verify and never refused", got.Error)
			}
			if n := runner.count("deploy"); n != test.WantRuns {
				t.Errorf("deploy ran %d times, want %d", n, test.WantRuns)
			}
			if n := len(runEntries(t, chain, r.ID, audit.MethodStart)); n != test.WantStarts {
				t.Errorf("chain holds %d starts, want %d", n, test.WantStarts)
			}
		})
	}
}

// TestAReleasedCoordinatorRestartAppendsNoSecondStart pins the restart path. The process that
// parked a workflow at its approval step stops, and another process holding the same key resumes it
// from the store: the step that already ran is settled from its result seal, not started again, so
// each step that ran holds exactly one start and the workflow and its approval step hold none.
func TestAReleasedCoordinatorRestartAppendsNoSecondStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, chain, key := run.NewMemStore(), audit.NewMemStore(), bndKey(t)
	first := &commandRecorder{}
	d1 := New(store, first, nil, WithNoJanitor(), WithAudits(chain), WithBindingKey(key),
		WithOwner("replica-one"))
	parent, err := d1.SubmitPipeline(ctx, "release", "", gatedWorkflow(0, false))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	node := waitParked(t, store, parent.ID)
	d1.Close()

	second := &commandRecorder{}
	d2 := New(store, second, nil, WithNoJanitor(), WithAudits(chain), WithBindingKey(key),
		WithOwner("replica-two"))
	defer d2.Close()
	if _, err := d2.DecideStep(ctx, node.ID, StepDecision{Approve: true, By: bndDecider()}); err != nil {
		t.Fatalf("DecideStep() error = %v", err)
	}
	if got := dispatchtest.WaitTerminal(t, store, parent.ID); got.Status != run.StatusSucceeded {
		t.Fatalf("workflow ended %s (%s), want succeeded", got.Status, got.Error)
	}
	if first.count("build") != 1 || second.count("build") != 0 || second.count("deploy") != 1 {
		t.Errorf("build ran %d then %d times and deploy %d times, want 1, 0, 1",
			first.count("build"), second.count("build"), second.count("deploy"))
	}
	steps, err := store.Steps(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Steps() error = %v", err)
	}
	for _, s := range append(steps, parent) {
		want := 1
		if s.Kind == run.KindApproval || s.ParentID == nil {
			want = 0
		}
		if n := len(runEntries(t, chain, s.ID, audit.MethodStart)); n != want {
			t.Errorf("%s %q holds %d starts, want %d", s.ID, s.StepName, n, want)
		}
	}
}
