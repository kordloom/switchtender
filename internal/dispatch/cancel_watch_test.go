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

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// watchDispatcher builds the smallest dispatcher the lease watcher needs, with a registered cancel
// for id whose context the caller can wait on.
func watchDispatcher(t *testing.T, store run.Store, owner, id string) (*Dispatcher, context.Context) {
	t.Helper()
	d := &Dispatcher{
		store:   store,
		log:     zap.NewNop(),
		owner:   owner,
		cancels: make(map[string]context.CancelFunc),
	}
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d.register(id, cancel)
	return d, runCtx
}

// TestWatchStopsARunCanceledByAnotherProcess pins the cross-process half of cancellation. A cancel
// arrives at whichever node serves the API, which is usually not the node executing the run, so the
// only thing that reaches the executor is a flag written to the shared store. The watcher is what
// reads it. Without this the API answers "canceling" while the tool keeps changing hosts, and the
// run finishes succeeded.
func TestWatchStopsARunCanceledByAnotherProcess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()

	claimed := time.Now()
	r := &run.Run{
		ID: "run_remote_cancel", Playbook: "site.yml", Status: run.StatusRunning,
		CreatedAt: claimed, StartedAt: &claimed, ClaimedBy: "worker-a", ClaimedAt: &claimed,
	}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	d, runCtx := watchDispatcher(t, store, "worker-a", r.ID)
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go d.watch(watchCtx, r.ID)

	// The other process records the cancel. Nothing else tells this executor about it.
	if err := store.RequestCancel(ctx, r.ID); err != nil {
		t.Fatalf("RequestCancel() error = %v", err)
	}

	select {
	case <-runCtx.Done():
	case <-time.After(3 * watchInterval):
		t.Fatal("the executing run was never stopped by a cancel another process wrote to the " +
			"store, so a cancel the API accepted stops nothing")
	}
}

// readFailingStore serves every call from the wrapped store but refuses reads once armed, standing in
// for a store that is reachable for writes and briefly not for reads.
type readFailingStore struct {
	run.Store
	// failing arms the read refusal.
	failing atomic.Bool
	// reads counts refused reads, so a test can prove the path was exercised.
	reads atomic.Int64
}

// Get refuses while armed and otherwise serves the wrapped store.
func (s *readFailingStore) Get(ctx context.Context, id string) (*run.Run, error) {
	if s.failing.Load() {
		s.reads.Add(1)
		return nil, errors.New("read timeout")
	}
	return s.Store.Get(ctx, id)
}

// TestWatchDoesNotStopARunOverAFailedCancelRead pins the other side of the same watcher. The lease is
// renewing, so nothing else can claim this run and it is provably still this executor's. A read that
// could not answer whether a cancel was requested is not a cancel, and killing a tool partway through
// its changes on the strength of an unanswered question is the failure mode the whole watcher was
// rewritten to avoid.
func TestWatchDoesNotStopARunOverAFailedCancelRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := run.NewMemStore()
	store := &readFailingStore{Store: base}

	claimed := time.Now()
	r := &run.Run{
		ID: "run_read_blip", Playbook: "site.yml", Status: run.StatusRunning,
		CreatedAt: claimed, StartedAt: &claimed, ClaimedBy: "worker-a", ClaimedAt: &claimed,
	}
	if err := base.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	store.failing.Store(true)

	d, runCtx := watchDispatcher(t, store, "worker-a", r.ID)
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go d.watch(watchCtx, r.ID)

	select {
	case <-runCtx.Done():
		t.Fatal("a healthy run holding a renewing lease was killed because one cancel read failed")
	case <-time.After(2 * watchInterval):
	}
	if store.reads.Load() == 0 {
		t.Error("the failing read was never exercised, so this proves nothing")
	}
}

// TestWatchStopsAtOnceWhenTheLeaseIsGone pins the one store answer that must stop a run immediately.
// ErrNotFound from a heartbeat means somebody else holds this run, and carrying on would mean two
// executors changing the same hosts at the same time. Unlike an unreachable store, there is nothing
// ambiguous about it.
func TestWatchStopsAtOnceWhenTheLeaseIsGone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()

	claimed := time.Now()
	r := &run.Run{
		ID: "run_lost_lease", Playbook: "site.yml", Status: run.StatusRunning,
		CreatedAt: claimed, StartedAt: &claimed, ClaimedBy: "worker-b", ClaimedAt: &claimed,
	}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// This dispatcher believes it owns the run, but the store says the lease belongs to worker-b.
	d, runCtx := watchDispatcher(t, store, "worker-a", r.ID)
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go d.watch(watchCtx, r.ID)

	select {
	case <-runCtx.Done():
	case <-time.After(3 * watchInterval):
		t.Fatal("an executor whose lease belongs to somebody else kept running, so two workers " +
			"change the same hosts at once")
	}
}

// TestWatchExitsWithItsRun pins the watcher's own lifetime. It is started per execution and must stop
// when the run's context ends, or every finished run leaves a goroutine heartbeating a lease forever.
func TestWatchExitsWithItsRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()

	claimed := time.Now()
	r := &run.Run{
		ID: "run_watch_exit", Playbook: "site.yml", Status: run.StatusRunning,
		CreatedAt: claimed, StartedAt: &claimed, ClaimedBy: "worker-a", ClaimedAt: &claimed,
	}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	d, _ := watchDispatcher(t, store, "worker-a", r.ID)
	watchCtx, stopWatch := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.watch(watchCtx, r.ID)
	}()
	stopWatch()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the lease watcher outlived its run, so a finished run keeps renewing its lease")
	}
}

// unreachableClaimStore refuses the compare-and-swap that starts a coordinated parent, standing in for
// a store the coordinator cannot reach at the moment it needs to claim.
type unreachableClaimStore struct {
	run.Store
	// attempts counts refused claims, so the retry budget is visibly spent.
	attempts atomic.Int64
}

// TransitionStatusAndClaim refuses every claim.
func (s *unreachableClaimStore) TransitionStatusAndClaim(
	context.Context, string, run.Status, run.Status, string,
) (bool, error) {
	s.attempts.Add(1)
	return false, errors.New("dial tcp 10.0.0.5:5432: connect: connection refused")
}

// TestParentMayStartSettlesEverythingWhenItCannotClaim pins the fail-closed half of the start fence.
// Starting a fan-out on an unknown state risks resurrecting a run somebody already canceled, so the
// coordinator refuses. What it must not do is refuse and walk away: leaving the parent running with
// canceled children and a closed stream is a half-state only a lease sweep would ever resolve, and
// only for a parent that happens to hold a lease.
func TestParentMayStartSettlesEverythingWhenItCannotClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := run.NewMemStore()
	store := &unreachableClaimStore{Store: base}
	pub := newCapturingPublisher()

	d := New(store, &countingRunnerLister{hosts: []string{"web01", "web02"}}, nil,
		WithNoJanitor(), WithPublisher(pub))
	defer d.Close()

	parentID := "run_unclaimable"
	parent := &run.Run{
		ID: parentID, Playbook: "site.yml", Kind: run.KindSplit, Status: run.StatusPending,
		CreatedAt: time.Now(),
	}
	if err := base.Save(ctx, parent); err != nil {
		t.Fatalf("Save(parent) error = %v", err)
	}
	idx, count := 0, 1
	child := &run.Run{
		ID: "run_unclaimable_c0", Playbook: "site.yml", Status: run.StatusPending,
		CreatedAt: time.Now(), ParentID: &parentID, ShardIndex: &idx, ShardCount: &count,
		Limit: "web01",
	}
	if err := base.Save(ctx, child); err != nil {
		t.Fatalf("Save(child) error = %v", err)
	}

	if d.parentMayStart(parent.Clone(), []string{child.ID}) {
		t.Fatal("the coordinator started a fan-out without ever establishing the parent's state")
	}
	if store.attempts.Load() < 2 {
		t.Errorf("the claim was attempted %d times, want it retried like every other write on this "+
			"path", store.attempts.Load())
	}

	gotParent, err := base.Get(ctx, parentID)
	if err != nil {
		t.Fatalf("Get(parent) error = %v", err)
	}
	if !gotParent.Status.Terminal() {
		t.Errorf("parent is %q, want a terminal state: a parent left running with canceled children "+
			"is a half-state nothing resolves", gotParent.Status)
	}
	if gotParent.Error == "" {
		t.Error("the settled parent records no reason, so nobody can tell why the split never ran")
	}

	gotChild, err := base.Get(ctx, child.ID)
	if err != nil {
		t.Fatalf("Get(child) error = %v", err)
	}
	if !gotChild.Status.Terminal() {
		t.Errorf("child is %q, want it settled: nothing will ever coordinate it", gotChild.Status)
	}

	var closed bool
	for _, id := range pub.closedIDs() {
		if id == parentID {
			closed = true
		}
	}
	if !closed {
		t.Error("the parent's stream was never closed, so a viewer tailing it waits forever")
	}
}

// TestCancelChildrenLeavesTerminalChildrenAlone pins the guard at the top of the cancel fan-out. A
// shard that already finished is evidence of what happened on its hosts, and overwriting a succeeded
// shard with a cancel because its siblings were stopped would rewrite that evidence.
func TestCancelChildrenLeavesTerminalChildrenAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, &countingRunnerLister{hosts: []string{"web01"}}, nil, WithNoJanitor())
	defer d.Close()

	parentID := "run_mixed"
	ended := time.Now()
	idx0, idx1, count := 0, 1, 2
	finished := &run.Run{
		ID: "run_mixed_c0", Playbook: "site.yml", Status: run.StatusSucceeded,
		CreatedAt: ended, EndedAt: &ended, ParentID: &parentID, ShardIndex: &idx0, ShardCount: &count,
	}
	waiting := &run.Run{
		ID: "run_mixed_c1", Playbook: "site.yml", Status: run.StatusPending,
		CreatedAt: ended, ParentID: &parentID, ShardIndex: &idx1, ShardCount: &count,
	}
	for _, r := range []*run.Run{finished, waiting} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}

	// A child id that does not exist at all must not stop the fan-out either.
	d.cancelChildren([]string{finished.ID, "run_mixed_missing", waiting.ID})

	gotFinished, err := store.Get(ctx, finished.ID)
	if err != nil {
		t.Fatalf("Get(finished) error = %v", err)
	}
	if gotFinished.Status != run.StatusSucceeded {
		t.Errorf("a finished shard was rewritten to %q, erasing what actually happened on its hosts",
			gotFinished.Status)
	}
	if gotFinished.CancelRequested {
		t.Error("a cancel was requested on a shard that had already finished")
	}

	gotWaiting, err := store.Get(ctx, waiting.ID)
	if err != nil {
		t.Fatalf("Get(waiting) error = %v", err)
	}
	if !gotWaiting.Status.Terminal() {
		t.Errorf("an unclaimed pending shard is still %q, so it waits for a coordinator that is gone",
			gotWaiting.Status)
	}
}

// TestStoppedStatusTellsAShutdownFromACancel pins the two-way distinction the whole interrupted state
// exists for. A restart must not write the record a person clicking cancel leaves, because a partial
// retry accepts interrupted and refuses canceled, and the audit chain keeps whichever one is written.
func TestStoppedStatusTellsAShutdownFromACancel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which stop happened.
		Name string
		// Cause is the cancellation cause on the dispatcher's own context.
		Cause error
		// WantStatus is the terminal status a stopped run should record.
		WantStatus run.Status
		// WantReason says whether a reason must accompany it.
		WantReason bool
	}{{ // Test 0: A person canceled, which speaks for itself and carries no reason text.
		Name: "a user cancel", Cause: context.Canceled,
		WantStatus: run.StatusCanceled, WantReason: false,
	}, { // Test 1: The server stopped, which is interrupted and must say so.
		Name: "a server shutdown", Cause: errShuttingDown,
		WantStatus: run.StatusInterrupted, WantReason: true,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancelCause(context.Background())
			d := &Dispatcher{ctx: ctx}
			cancel(test.Cause)

			if got := d.stoppedStatus(); got != test.WantStatus {
				t.Errorf("stoppedStatus() = %q, want %q", got, test.WantStatus)
			}
			if got := d.stoppedReason(); (got != "") != test.WantReason {
				t.Errorf("stoppedReason() = %q, want a reason present = %v", got, test.WantReason)
			}
		})
	}

	// An unstopped dispatcher reports a cancel, which is the safe reading: nothing here is a
	// shutdown until the shutdown cause is actually set.
	t.Run("test 2", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		d := &Dispatcher{ctx: ctx}
		if got := d.stoppedStatus(); got != run.StatusCanceled {
			t.Errorf("stoppedStatus() on a live dispatcher = %q, want canceled", got)
		}
	})
}

// TestSplitRollsUpACanceledShardAsCanceled pins the coordinator's rollup for the middle case. A split
// where one shard was canceled and the rest succeeded did not fail: nothing went wrong, somebody
// stopped part of it. Reporting that as failed would put a false failure on the chain for a change
// that was deliberately halted.
func TestSplitRollsUpACanceledShardAsCanceled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, &countingRunnerLister{hosts: []string{"web01", "web02"}}, nil, WithNoJanitor())
	defer d.Close()

	parentID := "run_rollup_cancel"
	parent := &run.Run{
		ID: parentID, Playbook: "site.yml", Kind: run.KindSplit, Status: run.StatusPending,
		CreatedAt: time.Now(),
	}
	if err := store.Save(ctx, parent); err != nil {
		t.Fatalf("Save(parent) error = %v", err)
	}
	ended := time.Now()
	idx0, idx1, count := 0, 1, 2
	children := []*run.Run{{
		ID: parentID + "_c0", Playbook: "site.yml", Status: run.StatusSucceeded,
		CreatedAt: ended, EndedAt: &ended, ParentID: &parentID, ShardIndex: &idx0, ShardCount: &count,
	}, {
		ID: parentID + "_c1", Playbook: "site.yml", Status: run.StatusCanceled,
		CreatedAt: ended, EndedAt: &ended, ParentID: &parentID, ShardIndex: &idx1, ShardCount: &count,
	}}
	for _, c := range children {
		if err := store.Save(ctx, c); err != nil {
			t.Fatalf("Save(%s) error = %v", c.ID, err)
		}
	}

	d.wg.Add(1)
	d.coordinate(parent.Clone(), children)

	got, err := store.Get(ctx, parentID)
	if err != nil {
		t.Fatalf("Get(parent) error = %v", err)
	}
	if got.Status != run.StatusCanceled {
		t.Errorf("split status = %q, want canceled: one shard was stopped on purpose and the rest "+
			"succeeded, so nothing failed", got.Status)
	}
}

// TestSplitRollsUpAnInterruptedShardAheadOfAFailure pins the precedence between the two. A shard the
// server stopped explains the whole split and is the state a partial retry accepts, while calling it
// failed would state an outcome the run never actually reached for the shards that never finished.
func TestSplitRollsUpAnInterruptedShardAheadOfAFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, &countingRunnerLister{hosts: []string{"web01", "web02"}}, nil, WithNoJanitor())
	defer d.Close()

	parentID := "run_rollup_interrupt"
	parent := &run.Run{
		ID: parentID, Playbook: "site.yml", Kind: run.KindSplit, Status: run.StatusPending,
		CreatedAt: time.Now(),
	}
	if err := store.Save(ctx, parent); err != nil {
		t.Fatalf("Save(parent) error = %v", err)
	}
	ended := time.Now()
	idx0, idx1, idx2, count := 0, 1, 2, 3
	children := []*run.Run{{
		ID: parentID + "_c0", Playbook: "site.yml", Status: run.StatusFailed,
		CreatedAt: ended, EndedAt: &ended, ParentID: &parentID, ShardIndex: &idx0, ShardCount: &count,
	}, {
		ID: parentID + "_c1", Playbook: "site.yml", Status: run.StatusInterrupted,
		CreatedAt: ended, EndedAt: &ended, ParentID: &parentID, ShardIndex: &idx1, ShardCount: &count,
	}, {
		ID: parentID + "_c2", Playbook: "site.yml", Status: run.StatusCanceled,
		CreatedAt: ended, EndedAt: &ended, ParentID: &parentID, ShardIndex: &idx2, ShardCount: &count,
	}}
	for _, c := range children {
		if err := store.Save(ctx, c); err != nil {
			t.Fatalf("Save(%s) error = %v", c.ID, err)
		}
	}

	d.wg.Add(1)
	d.coordinate(parent.Clone(), children)

	got, err := store.Get(ctx, parentID)
	if err != nil {
		t.Fatalf("Get(parent) error = %v", err)
	}
	if got.Status != run.StatusInterrupted {
		t.Errorf("split status = %q, want interrupted: an interrupted shard outranks both a failure "+
			"and a cancel, and it is the only state a partial retry accepts", got.Status)
	}
}

// blockingRunner blocks every execution until released, recording how many are in flight at once so a
// test can read the real concurrency the worker pool allowed.
type blockingRunner struct {
	// release is closed to let every blocked execution finish.
	release chan struct{}
	// started is signaled once per execution that reached the runner.
	started chan struct{}
	// mu guards live and peak.
	mu sync.Mutex
	// live is how many executions are in the runner right now.
	live int
	// peak is the most that were ever in it at once.
	peak int
}

// newBlockingRunner returns a runner that holds up to capacity concurrent executions.
func newBlockingRunner(capacity int) *blockingRunner {
	return &blockingRunner{
		release: make(chan struct{}),
		started: make(chan struct{}, capacity),
	}
}

// Run records its arrival and blocks until the test releases it or the run is canceled.
func (b *blockingRunner) Run(ctx context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
	b.mu.Lock()
	b.live++
	if b.live > b.peak {
		b.peak = b.live
	}
	b.mu.Unlock()
	select {
	case b.started <- struct{}{}:
	default:
	}
	defer func() {
		b.mu.Lock()
		b.live--
		b.mu.Unlock()
	}()
	select {
	case <-b.release:
		return roundhouse.Result{ExitCode: 0}, nil
	case <-ctx.Done():
		return roundhouse.Result{ExitCode: -1}, ctx.Err()
	}
}

// highWater returns the most executions that were ever in flight at once.
func (b *blockingRunner) highWater() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.peak
}

// TestWorkerPoolBoundsConcurrentExecutions pins the resource bound the whole dispatcher rests on. The
// pool size is what stops one burst of submissions from opening a connection to every host in the
// fleet at once, and it is enforced by a semaphore the claim loop takes before it claims. A pool that
// let one extra run through would be invisible in every single-run test.
func TestWorkerPoolBoundsConcurrentExecutions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Workers is the configured pool size.
		Workers int
		// Submit is how many runs are queued at once.
		Submit int
	}{
		{Workers: 1, Submit: 6},  // Test 0: A single worker serializes everything.
		{Workers: 2, Submit: 8},  // Test 1: Two workers, well over-subscribed.
		{Workers: 3, Submit: 12}, // Test 2: And three.
	}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			runner := newBlockingRunner(test.Submit)
			d := New(store, runner, nil, WithNoJanitor(), WithWorkers(test.Workers),
				WithClaimInterval(time.Millisecond))

			ids := make([]string, 0, test.Submit)
			for i := range test.Submit {
				created, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash),
					run.WithCommand(fmt.Sprintf("echo %d", i)))
				if err != nil {
					t.Fatalf("Submit(%d) error = %v", i, err)
				}
				ids = append(ids, created.ID)
			}

			// Wait until the pool is saturated, then give it room to overshoot if it is going to.
			deadline := time.Now().Add(10 * time.Second)
			for runner.highWater() < test.Workers && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			time.Sleep(150 * time.Millisecond)

			if got := runner.highWater(); got > test.Workers {
				t.Errorf("%d runs executed at once with a pool of %d, so the bound that keeps a "+
					"burst off the whole fleet does not hold", got, test.Workers)
			}
			if got := runner.highWater(); got < test.Workers {
				t.Errorf("only %d runs executed at once with a pool of %d and %d queued, so the "+
					"pool is under-used", got, test.Workers, test.Submit)
			}

			close(runner.release)
			for _, id := range ids {
				waitTerminal(t, store, id)
			}
			d.Close()
		})
	}
}

// TestCloseStopsEveryRunItHasInFlight pins shutdown across a saturated pool. Every executing run has
// to be stopped and recorded, not left running with no process watching it, and the record has to say
// the server stopped rather than that somebody canceled.
func TestCloseStopsEveryRunItHasInFlight(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	runner := newBlockingRunner(4)
	d := New(store, runner, nil, WithNoJanitor(), WithWorkers(3),
		WithClaimInterval(time.Millisecond))

	ids := make([]string, 0, 3)
	for i := range 3 {
		created, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash),
			run.WithCommand(fmt.Sprintf("sleep %d", i)))
		if err != nil {
			t.Fatalf("Submit(%d) error = %v", i, err)
		}
		ids = append(ids, created.ID)
	}
	deadline := time.Now().Add(10 * time.Second)
	for runner.highWater() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if runner.highWater() < 3 {
		t.Fatalf("only %d runs ever started, so the shutdown is not landing mid-run",
			runner.highWater())
	}

	d.Close()

	for _, id := range ids {
		got, err := store.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", id, err)
		}
		if !got.Status.Terminal() {
			t.Errorf("run %s is still %q after Close drained, so it is running with nothing "+
				"watching it", id, got.Status)
		}
		if got.Status == run.StatusCanceled {
			t.Errorf("run %s was recorded canceled, which is what a person clicking cancel leaves; "+
				"a restart must record interrupted so a partial retry can recover it", id)
		}
	}
}
