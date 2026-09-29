package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// blindStore accepts nothing conditionally and answers no read, standing in for a store that is
// reachable enough to be called and unable to tell the caller anything true.
type blindStore struct {
	run.Store
	// saves counts whole-row writes, which must not happen when the state is unknown.
	saves atomic.Int64
}

// FinalizeRunning reports that it changed nothing, without an error, which is what a lost lease looks
// like.
func (s *blindStore) FinalizeRunning(context.Context, string, run.Finalization) (bool, error) {
	return false, nil
}

// Get refuses every read, however many times it is retried.
func (s *blindStore) Get(context.Context, string) (*run.Run, error) {
	return nil, errors.New("dial tcp 10.0.0.5:5432: connect: connection refused")
}

// Save counts any whole-row write that gets past the read.
func (s *blindStore) Save(ctx context.Context, r *run.Run) error {
	s.saves.Add(1)
	return s.Store.Save(ctx, r)
}

// TestFinalizeWritesNothingWhenTheStoredStateIsUnknown pins the last fence before a terminal write.
// The conditional write changed nothing, which means this executor may no longer hold the run, and
// the read that would settle it cannot be answered. Writing anyway risks terminalizing a live run
// another worker claimed after the janitor requeued this one, which is precisely what the fence
// exists to stop. Skipping leaves the run for the sweep, which is recoverable.
func TestFinalizeWritesNothingWhenTheStoredStateIsUnknown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mem := run.NewMemStore()
	store := &blindStore{Store: mem}
	audits := audit.NewMemStore()
	d := New(store, okRunner(), nil, WithNoJanitor(), WithAudits(audits))
	defer d.Close()

	started := time.Now()
	r := &run.Run{
		ID: "run_blind", Playbook: "site.yml", Inventory: "inv", Status: run.StatusRunning,
		CreatedAt: started, StartedAt: &started, ClaimedBy: "worker-1", ClaimedAt: &started,
		Actor: "casey",
	}
	if err := mem.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	store.saves.Store(0)

	code := 0
	d.finalize(r, run.StatusSucceeded, &code, "")

	if r.Status == run.StatusSucceeded {
		t.Error("the run in memory reports succeeded while the store was never told, so this " +
			"executor now believes an outcome the database does not hold")
	}
	if got := store.saves.Load(); got != 0 {
		t.Errorf("%d whole-row writes on an unknown state: writing blind can terminalize a live run "+
			"another worker is executing", got)
	}

	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	for _, e := range chain {
		if e.Method == audit.MethodRun {
			t.Errorf("an outcome was committed for a run the store never recorded: %q", e.Path)
		}
	}
}

// stepBlockingRunner blocks the named step until released, so a test can cancel a pipeline while one
// step is genuinely executing, and records which steps ever reached the runner.
type stepBlockingRunner struct {
	// blockOn is the command whose execution blocks.
	blockOn string
	// reached is signaled when the blocking step starts.
	reached chan struct{}
	// mu guards seen.
	mu sync.Mutex
	// seen lists the commands that reached the runner.
	seen []string
}

// Run blocks on the named step until the run is canceled and succeeds for every other.
func (s *stepBlockingRunner) Run(ctx context.Context, spec roundhouse.Spec,
	_ io.Writer) (roundhouse.Result, error) {
	s.mu.Lock()
	s.seen = append(s.seen, spec.Command)
	s.mu.Unlock()
	if spec.Command != s.blockOn {
		return roundhouse.Result{ExitCode: 0}, nil
	}
	select {
	case s.reached <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return roundhouse.Result{ExitCode: -1}, ctx.Err()
}

// commands returns the step commands that reached the runner.
func (s *stepBlockingRunner) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

// TestCancelingAPipelineStopsTheStepsThatFollow pins cancellation through a multi-step workflow. The
// parent registers its own cancel so stopping it stops the step in flight and halts everything that
// has not started. A pipeline that carried on after a cancel would keep changing hosts after the API
// had already answered that it was stopping.
func TestCancelingAPipelineStopsTheStepsThatFollow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	runner := &stepBlockingRunner{blockOn: "step-two", reached: make(chan struct{}, 1)}
	d := New(store, runner, nil, WithNoJanitor(), WithClaimInterval(time.Millisecond))
	defer d.Close()

	parent, err := d.SubmitPipeline(ctx, "release", "inv", []run.PipelineStep{
		{Name: "one", Tool: run.ToolBash, Command: "step-one"},
		{Name: "two", Tool: run.ToolBash, Command: "step-two"},
		{Name: "three", Tool: run.ToolBash, Command: "step-three"},
	})
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}

	select {
	case <-runner.reached:
	case <-time.After(20 * time.Second):
		t.Fatal("the pipeline never reached its second step, so the cancel would not land mid-run")
	}

	if !d.Cancel(parent.ID) {
		t.Fatal("the running pipeline was not registered as cancelable")
	}

	got := waitTerminal(t, store, parent.ID)
	if got.Status != run.StatusCanceled {
		t.Errorf("pipeline status = %q, want canceled", got.Status)
	}
	for _, cmd := range runner.commands() {
		if cmd == "step-three" {
			t.Error("a step after the cancel still executed, so the pipeline kept changing hosts " +
				"after the API answered that it was stopping")
		}
	}
	steps, err := store.Steps(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Steps() error = %v", err)
	}
	for _, s := range steps {
		if s.StepName == "three" {
			t.Error("a run was created for a step the cancel should have prevented")
		}
	}
}

// TestRunStepAttemptsStopsWithoutCreatingARun pins the check at the top of each attempt. A pipeline
// canceled between steps must create no further step runs at all: a stored step run is claimable, so
// one created after the cancel would be picked up and executed by any worker sharing the store.
func TestRunStepAttemptsStopsWithoutCreatingARun(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	store := run.NewMemStore()
	runner := &countingRunnerLister{hosts: []string{"web01"}}
	d := New(store, runner, nil, WithNoJanitor())
	defer d.Close()

	parent := &run.Run{
		ID: "run_pipe_canceled", Playbook: "release", Inventory: "inv", Kind: run.KindPipeline,
		Status: run.StatusRunning, CreatedAt: time.Now(),
	}
	if err := store.Save(context.Background(), parent); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	status, outputs := d.runStepAttempts(ctx, parent,
		run.PipelineStep{Name: "one", Tool: run.ToolBash, Command: "echo hi", Retries: 3}, 0, nil)

	if status != run.StatusCanceled {
		t.Errorf("status = %q, want canceled", status)
	}
	if outputs != nil {
		t.Errorf("outputs = %v, want none from a step that never ran", outputs)
	}
	if n := runner.executions.Load(); n != 0 {
		t.Errorf("%d executions from a canceled pipeline", n)
	}
	steps, err := store.Steps(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Steps() error = %v", err)
	}
	if len(steps) != 0 {
		t.Errorf("%d step runs were stored after the cancel; each one is claimable, so any worker "+
			"sharing the store would execute it", len(steps))
	}
}

// stepSaveFailStore refuses to store pipeline step runs.
type stepSaveFailStore struct {
	run.Store
	// refused counts refused step saves.
	refused atomic.Int64
}

// Save refuses any run that names a step index and serves every other write.
func (s *stepSaveFailStore) Save(ctx context.Context, r *run.Run) error {
	if r.StepIndex != nil {
		s.refused.Add(1)
		return errors.New("database is locked")
	}
	return s.Store.Save(ctx, r)
}

// TestAStepThatCannotBeStoredFailsRatherThanRunningUnrecorded pins what happens when a step run
// cannot be written. Execution happens through the claim loop, which reads the stored row, so a step
// that was never stored can never run; reporting anything but a failure would let the pipeline walk
// past a step that did nothing and call the workflow done.
func TestAStepThatCannotBeStoredFailsRatherThanRunningUnrecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := run.NewMemStore()
	store := &stepSaveFailStore{Store: base}
	runner := &countingRunnerLister{hosts: []string{"web01"}}
	d := New(store, runner, zap.NewNop(), WithNoJanitor(), WithClaimInterval(time.Millisecond))
	defer d.Close()

	parent := &run.Run{
		ID: "run_pipe_nostore", Playbook: "release", Inventory: "inv", Kind: run.KindPipeline,
		Status: run.StatusRunning, CreatedAt: time.Now(),
	}
	if err := base.Save(ctx, parent); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	status, outputs := d.runStepAttempts(ctx, parent,
		run.PipelineStep{Name: "one", Tool: run.ToolBash, Command: "echo hi"}, 0, nil)

	if store.refused.Load() == 0 {
		t.Fatal("the store was never asked to record the step")
	}
	if status != run.StatusFailed {
		t.Errorf("status = %q, want failed: a step nothing stored can never be claimed and run",
			status)
	}
	if outputs != nil {
		t.Errorf("outputs = %v, want none", outputs)
	}
	if n := runner.executions.Load(); n != 0 {
		t.Errorf("%d executions for a step that was never stored", n)
	}
}

// shardListFailStore refuses the parent-scoped shard listing a coordinator polls with.
type shardListFailStore struct {
	run.Store
	// refused counts refused listings, so a test can prove the fallback was needed.
	refused atomic.Int64
}

// Shards refuses every listing.
func (s *shardListFailStore) Shards(context.Context, string) ([]*run.Run, error) {
	s.refused.Add(1)
	return nil, errors.New("database is locked")
}

// TestCoordinatorFallsBackToPointReadsWhenTheShardListingFails pins the coordinator's read path under
// a partial store fault. The parent-scoped query exists so a large fan-out does not issue hundreds of
// point reads per tick, but it must be an optimization rather than the only way to see a child
// finish: without the fallback a failing listing would leave the coordinator polling forever while
// every shard had already completed.
func TestCoordinatorFallsBackToPointReadsWhenTheShardListingFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := run.NewMemStore()
	store := &shardListFailStore{Store: base}
	d := New(store, &countingRunnerLister{hosts: []string{"web01", "web02"}}, nil, WithNoJanitor())
	defer d.Close()

	parentID := "run_fallback"
	ended := time.Now()
	idx0, idx1, count := 0, 1, 2
	ids := []string{parentID + "_c0", parentID + "_c1"}
	for i, id := range ids {
		idx := idx0
		if i == 1 {
			idx = idx1
		}
		if err := base.Save(ctx, &run.Run{
			ID: id, Playbook: "site.yml", Status: run.StatusSucceeded, CreatedAt: ended,
			EndedAt: &ended, ParentID: &parentID, ShardIndex: &idx, ShardCount: &count,
		}); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
	}

	done := make(chan []run.Status, 1)
	go func() { done <- d.waitChildren(ctx, ids) }()

	select {
	case statuses := <-done:
		for i, s := range statuses {
			if s != run.StatusSucceeded {
				t.Errorf("child %d reported %q, want succeeded", i, s)
			}
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the coordinator never saw its children finish, so a failing shard listing leaves " +
			"a split running forever with every shard already done")
	}
	if store.refused.Load() == 0 {
		t.Error("the parent-scoped listing was never attempted, so the fallback was not exercised")
	}
}

// TestWaitChildrenReturnsImmediatelyForFinishedChildren pins the poll's fast path. Every shard being
// terminal already is the ordinary case after a restart, and a coordinator that still slept a poll
// interval per child would add half a second per tick to every recovered split.
func TestWaitChildrenReturnsImmediatelyForFinishedChildren(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, &countingRunnerLister{hosts: []string{"web01"}}, nil, WithNoJanitor())
	defer d.Close()

	parentID := "run_finished"
	ended := time.Now()
	want := []run.Status{run.StatusSucceeded, run.StatusFailed, run.StatusCanceled}
	ids := make([]string, len(want))
	count := len(want)
	for i, status := range want {
		idx := i
		ids[i] = fmt.Sprintf("%s_c%d", parentID, i)
		if err := store.Save(ctx, &run.Run{
			ID: ids[i], Playbook: "site.yml", Status: status, CreatedAt: ended, EndedAt: &ended,
			ParentID: &parentID, ShardIndex: &idx, ShardCount: &count,
		}); err != nil {
			t.Fatalf("Save(%s) error = %v", ids[i], err)
		}
	}

	start := time.Now()
	got := d.waitChildren(ctx, ids)
	if elapsed := time.Since(start); elapsed > childPollInterval {
		t.Errorf("waiting on already finished children took %v, want no poll at all", elapsed)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("child %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestWaitChildrenReportsAStoppedChildHonestly pins what the coordinator records for children that
// never finished. It reports them stopped rather than waiting on a store that may be closing, and it
// says which stop it was: a cancel a person asked for, or the server going down, which is the only
// state a partial retry accepts.
func TestWaitChildrenReportsAStoppedChildHonestly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says which stop happened.
		Name string
		// Cause is the cancellation cause on the dispatcher's own context.
		Cause error
		// WantStatus is what an unfinished child should be reported as.
		WantStatus run.Status
	}{{ // Test 0: A person canceled the parent.
		Name: "a user cancel", Cause: context.Canceled, WantStatus: run.StatusCanceled,
	}, { // Test 1: The server stopped underneath it.
		Name: "a shutdown", Cause: errShuttingDown, WantStatus: run.StatusInterrupted,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			d := New(store, &countingRunnerLister{hosts: []string{"web01"}}, nil, WithNoJanitor())
			defer d.Close()
			d.cancel(test.Cause)

			parentID := fmt.Sprintf("run_stopped_%d", testNum)
			idx, count := 0, 1
			child := &run.Run{
				ID: parentID + "_c0", Playbook: "site.yml", Status: run.StatusPending,
				CreatedAt: time.Now(), ParentID: &parentID, ShardIndex: &idx, ShardCount: &count,
			}
			if err := store.Save(ctx, child); err != nil {
				t.Fatalf("Save() error = %v", err)
			}

			stopped, cancelStopped := context.WithCancel(context.Background())
			cancelStopped()
			got := d.waitChildren(stopped, []string{child.ID})

			if len(got) != 1 || got[0] != test.WantStatus {
				t.Errorf("reported %v, want [%q]", got, test.WantStatus)
			}
			stored, err := store.Get(ctx, child.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if !stored.Status.Terminal() {
				t.Errorf("the unclaimed child is still %q, so it waits for a coordinator that has "+
					"already given up on it", stored.Status)
			}
		})
	}
}
