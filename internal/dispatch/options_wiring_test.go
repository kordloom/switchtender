package dispatch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
)

// TestNewNormalizesItsOptions pins what New installs for each option, rather than what the rules
// those options feed do afterwards. A constructor that stores an out-of-range value, or drops one
// entirely, passes every behavioral test written against the default and then quietly runs the whole
// fleet on the wrong bound. Each case here reads the field the option was meant to reach.
func TestNewNormalizesItsOptions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which bound is being pinned.
		Name string
		// Opts are the options passed to New.
		Opts []Option
		// WantWorkers is the worker slot count the semaphore must hold.
		WantWorkers int
		// WantMaxShards is the shard ceiling a split is clamped to.
		WantMaxShards int
		// WantClaimInterval is the idle poll base.
		WantClaimInterval time.Duration
		// WantRunTimeout is the per-run cap; zero means no cap.
		WantRunTimeout time.Duration
		// WantQueues is the queue set this process serves.
		WantQueues []string
	}{{ // Test 0: No options at all leaves every documented default in place.
		Name: "bare defaults", WantWorkers: DefaultWorkers, WantMaxShards: DefaultMaxShards,
		WantClaimInterval: DefaultClaimInterval, WantRunTimeout: 0, WantQueues: []string{""},
	}, { // Test 1: Zero is below one everywhere, so every bound falls back rather than becoming zero.
		Name: "zero everywhere",
		Opts: []Option{WithWorkers(0), WithMaxShards(0), WithClaimInterval(0), WithRunTimeout(0),
			WithQueues(nil)},
		WantWorkers: DefaultWorkers, WantMaxShards: DefaultMaxShards,
		WantClaimInterval: DefaultClaimInterval, WantRunTimeout: 0, WantQueues: []string{""},
	}, { // Test 2: Negative values are refused the same way, so no bound can be inverted.
		Name: "negative everywhere",
		Opts: []Option{WithWorkers(-5), WithMaxShards(-1), WithClaimInterval(-time.Second),
			WithRunTimeout(-time.Hour), WithQueues([]string{})},
		WantWorkers: DefaultWorkers, WantMaxShards: DefaultMaxShards,
		WantClaimInterval: DefaultClaimInterval, WantRunTimeout: 0, WantQueues: []string{""},
	}, { // Test 3: One is the smallest legal worker pool and the smallest legal shard ceiling.
		Name: "the smallest legal values",
		Opts: []Option{WithWorkers(1), WithMaxShards(1), WithClaimInterval(time.Nanosecond),
			WithRunTimeout(time.Nanosecond)},
		WantWorkers: 1, WantMaxShards: 1, WantClaimInterval: time.Nanosecond,
		WantRunTimeout: time.Nanosecond, WantQueues: []string{""},
	}, { // Test 4: Ordinary settings reach the fields untouched.
		Name: "ordinary settings",
		Opts: []Option{WithWorkers(9), WithMaxShards(7), WithClaimInterval(40 * time.Millisecond),
			WithRunTimeout(90 * time.Second), WithQueues([]string{"eu", "us"})},
		WantWorkers: 9, WantMaxShards: 7, WantClaimInterval: 40 * time.Millisecond,
		WantRunTimeout: 90 * time.Second, WantQueues: []string{"eu", "us"},
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			opts := append([]Option{WithNoJanitor()}, test.Opts...)
			d := New(run.NewMemStore(), okRunner(), nil, opts...)
			defer d.Close()

			if got := cap(d.sem); got != test.WantWorkers {
				t.Errorf("worker slots = %d, want %d: the pool bound is what stops one submission "+
					"from executing on every host at once", got, test.WantWorkers)
			}
			if d.maxShards != test.WantMaxShards {
				t.Errorf("maxShards = %d, want %d", d.maxShards, test.WantMaxShards)
			}
			if d.claimInterval != test.WantClaimInterval {
				t.Errorf("claimInterval = %v, want %v", d.claimInterval, test.WantClaimInterval)
			}
			if d.runTimeout != test.WantRunTimeout {
				t.Errorf("runTimeout = %v, want %v: a negative cap must disable the bound rather "+
					"than expire every run immediately", d.runTimeout, test.WantRunTimeout)
			}
			if diff := cmp.Diff(test.WantQueues, d.queues, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("queues (-want +got):\n%s", diff)
			}
		})
	}
}

// TestNewFillsTheRemainingDefaults covers the constructor's non-numeric fallbacks: the discard
// publisher, the process owner name, and the wall clock. A nil clock left in place panics on the
// first run rather than failing a rule, and a nil publisher would do the same on the first chunk of
// output, so both are worth reading directly.
func TestNewFillsTheRemainingDefaults(t *testing.T) {
	t.Parallel()
	d := New(run.NewMemStore(), okRunner(), nil, WithNoJanitor())
	defer d.Close()

	if _, ok := d.publisher.(noopPublisher); !ok {
		t.Errorf("publisher = %T, want the discarding default", d.publisher)
	}
	if d.owner == "" {
		t.Error("owner is empty, so nothing this process leases is attributable to it")
	}
	if d.Owner() != d.owner {
		t.Errorf("Owner() = %q, want the stamped owner %q", d.Owner(), d.owner)
	}
	if d.now == nil {
		t.Fatal("the clock is nil, so the first run to be stamped panics")
	}
	if got := d.now(); time.Since(got) > time.Minute || got.After(time.Now().Add(time.Minute)) {
		t.Errorf("the default clock read %v, want roughly now", got)
	}
	if d.cancels == nil {
		t.Error("the cancel register is nil, so no run can ever be stopped by id")
	}
	if cap(d.wakeCh) != 1 {
		t.Errorf("wake channel capacity = %d, want 1: one pending token already means there is "+
			"work to claim", cap(d.wakeCh))
	}
}

// TestWithClockOverridesTheStampedTime proves the clock option actually reaches the timestamps a run
// carries, and that passing nil restores the real clock rather than installing a nil function. The
// demo relies on this to seed runs whose record, chain entry, and receipt all agree on a past
// instant.
func TestWithClockOverridesTheStampedTime(t *testing.T) {
	t.Parallel()
	parked := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	store := run.NewMemStore()
	d := New(store, okRunner(), nil, WithNoJanitor(), WithClock(func() time.Time { return parked }))
	defer d.Close()

	created, err := d.Submit(context.Background(), "", "",
		run.WithTool(run.ToolBash), run.WithCommand("echo hi"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if !created.CreatedAt.Equal(parked) {
		t.Errorf("CreatedAt = %v, want the parked clock %v", created.CreatedAt, parked)
	}

	back := New(run.NewMemStore(), okRunner(), nil, WithNoJanitor(), WithClock(nil))
	defer back.Close()
	if back.now == nil {
		t.Fatal("a nil clock was installed rather than restoring the real one")
	}
	if got := back.now(); time.Since(got) > time.Minute {
		t.Errorf("the restored clock read %v, want roughly now", got)
	}
}

// queueRecorder records the queue set every claim was made with, so a test can prove WithQueues
// reaches the store rather than only being stored on the dispatcher.
type queueRecorder struct {
	run.Store
	// mu guards seen.
	mu sync.Mutex
	// seen holds each queue set the claim loop asked for.
	seen [][]string
}

// Claim records the queues it was asked for and reports nothing pending.
func (q *queueRecorder) Claim(_ context.Context, _ string, queues []string) (*run.Run, error) {
	q.mu.Lock()
	q.seen = append(q.seen, append([]string(nil), queues...))
	q.mu.Unlock()
	return nil, run.ErrNonePending
}

// queues returns a copy of the recorded queue sets.
func (q *queueRecorder) queues() [][]string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([][]string(nil), q.seen...)
}

// TestWithQueuesReachesTheClaim pins the wiring between the option and the claim it governs. A worker
// given named queues must ask for exactly those: asking for the default pool instead would have a
// dedicated worker quietly execute the whole fleet's ungated work, which is the opposite of what
// naming a queue is for.
func TestWithQueuesReachesTheClaim(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the process was configured to serve.
		Name string
		// Opts configure the dispatcher.
		Opts []Option
		// WantQueues is the queue set every claim must carry.
		WantQueues []string
	}{{ // Test 0: Unset serves the default pool, which the store names with the empty string.
		Name: "unset serves the default pool", WantQueues: []string{""},
	}, { // Test 1: Named queues are passed through in order.
		Name: "named queues", Opts: []Option{WithQueues([]string{"gpu", "eu-west"})},
		WantQueues: []string{"gpu", "eu-west"},
	}, { // Test 2: A single named queue never silently gains the default pool alongside it.
		Name: "one named queue", Opts: []Option{WithQueues([]string{"gpu"})},
		WantQueues: []string{"gpu"},
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := &queueRecorder{Store: run.NewMemStore()}
			opts := append([]Option{WithNoJanitor(), WithClaimInterval(time.Millisecond)}, test.Opts...)
			d := New(store, okRunner(), nil, opts...)

			deadline := time.Now().Add(5 * time.Second)
			for len(store.queues()) == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			d.Close()

			seen := store.queues()
			if len(seen) == 0 {
				t.Fatal("the claim loop never polled the store")
			}
			for i, got := range seen {
				if diff := cmp.Diff(test.WantQueues, got, cmpopts.EquateEmpty()); diff != "" {
					t.Fatalf("claim %d queues (-want +got):\n%s", i, diff)
				}
			}
		})
	}
}

// TestWithOwnerStampsTheLease proves the owner option reaches the lease the store grants, not just
// the Owner accessor. Two workers sharing a store are told apart by this name alone, and the
// finalize fence compares it, so an owner that never reached the claim would let one worker
// terminalize another's live run.
func TestWithOwnerStampsTheLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, okRunner(), nil, WithNoJanitor(), WithOwner("worker-tokyo-7"),
		WithClaimInterval(time.Millisecond))
	defer d.Close()

	if d.Owner() != "worker-tokyo-7" {
		t.Fatalf("Owner() = %q, want worker-tokyo-7", d.Owner())
	}
	created, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("echo hi"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	got := waitTerminal(t, store, created.ID)
	if got.ClaimedBy != "worker-tokyo-7" {
		t.Errorf("ClaimedBy = %q, want worker-tokyo-7: the lease must name the process that took it",
			got.ClaimedBy)
	}
}

// sweepCounter counts stale-lease sweeps so a test can prove the janitor did or did not start.
type sweepCounter struct {
	run.Store
	// mu guards sweeps.
	mu sync.Mutex
	// sweeps counts calls to ReclaimStale.
	sweeps int
}

// ReclaimStale counts the sweep and reclaims nothing.
func (s *sweepCounter) ReclaimStale(context.Context, time.Duration) (int, error) {
	s.mu.Lock()
	s.sweeps++
	s.mu.Unlock()
	return 0, nil
}

// count returns how many sweeps have run.
func (s *sweepCounter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sweeps
}

// TestWithNoJanitorInstallsNoSweep pins the option a relay worker depends on. A worker runs against a
// store that cannot reclaim leases, so sweeping from there would be a worker deciding another
// process's runs are dead. The default must still sweep immediately on start, which is what covers a
// restart.
func TestWithNoJanitorInstallsNoSweep(t *testing.T) {
	t.Parallel()

	t.Run("test 0", func(t *testing.T) {
		t.Parallel()
		// Test 0: The default dispatcher sweeps once immediately, covering a restart.
		store := &sweepCounter{Store: run.NewMemStore()}
		d := New(store, okRunner(), nil, WithClaimInterval(time.Millisecond))
		deadline := time.Now().Add(5 * time.Second)
		for store.count() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		d.Close()
		if store.count() == 0 {
			t.Error("the janitor never swept, so a run stranded by a crashed worker is never recovered")
		}
	})

	t.Run("test 1", func(t *testing.T) {
		t.Parallel()
		// Test 1: WithNoJanitor sweeps nothing at all, however long it is left running.
		store := &sweepCounter{Store: run.NewMemStore()}
		d := New(store, okRunner(), nil, WithNoJanitor(), WithClaimInterval(time.Millisecond))
		time.Sleep(250 * time.Millisecond)
		d.Close()
		if got := store.count(); got != 0 {
			t.Errorf("%d sweeps ran with the janitor disabled, so a worker is reclaiming leases it "+
				"does not own", got)
		}
	})
}

// TestNoopPublisherDiscardsEverything pins the default publisher's contract. It is installed whenever
// no streaming is configured and is called on every chunk of every run, so a method that panicked on
// a nil slice or an empty id would take down the executor rather than merely dropping output.
func TestNoopPublisherDiscardsEverything(t *testing.T) {
	t.Parallel()
	var p Publisher = noopPublisher{}
	p.PublishEvents("", nil)
	p.PublishEvents("run_1", []event.Event{{Type: "play_start"}})
	p.PublishLog("", nil)
	p.PublishLog("run_1", []byte("output"))
	p.CloseRun("")
	p.CloseRun("run_1")
}

// TestNewPanicsWithAStatedReason pins the two dependencies with no sensible default. A nil store or
// runner is a wiring mistake in the process that built the dispatcher, and failing at construction
// names it; the alternative is a nil dereference on the first submitted run, far from the cause.
func TestNewPanicsWithAStatedReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which dependency was omitted.
		Name string
		// Build constructs the dispatcher that should panic.
		Build func()
		// WantMessage is the panic text.
		WantMessage string
	}{{ // Test 0: No store to persist runs into.
		Name:        "no store",
		Build:       func() { New(nil, okRunner(), nil) },
		WantMessage: "dispatch: Store required",
	}, { // Test 1: No runner to execute with.
		Name:        "no runner",
		Build:       func() { New(run.NewMemStore(), nil, nil) },
		WantMessage: "dispatch: Runner required",
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			defer func() {
				got := recover()
				if got == nil {
					t.Fatalf("test %d: New returned a dispatcher with a nil dependency", testNum)
				}
				if msg, ok := got.(string); !ok || msg != test.WantMessage {
					t.Errorf("panic = %v, want %q", got, test.WantMessage)
				}
			}()
			test.Build()
		})
	}
}

// TestIdleWaitStaysWithinItsJitterWindow pins the backoff arithmetic at its boundaries. The wait is
// half the interval plus a random share of it, doubling until the shift ceiling, so an idle
// dispatcher stops competing for the store's single writer without ever waiting unboundedly. An
// off-by-one in the shift silently multiplies submit-to-start latency for every user.
func TestIdleWaitStaysWithinItsJitterWindow(t *testing.T) {
	t.Parallel()
	const base = 200 * time.Millisecond
	d := &Dispatcher{claimInterval: base}

	tests := []struct {
		// Idle is how many consecutive claims came back empty.
		Idle int
		// WantBase is the un-jittered wait the idle count should produce.
		WantBase time.Duration
	}{
		{Idle: -1, WantBase: base},          // Test 0: A nonsense count never shifts below the base.
		{Idle: 0, WantBase: base},           // Test 1: Zero idles is the base interval.
		{Idle: 1, WantBase: base},           // Test 2: The first empty claim has not doubled yet.
		{Idle: 2, WantBase: 2 * base},       // Test 3: The second doubles once.
		{Idle: 4, WantBase: 8 * base},       // Test 4: The shift ceiling is reached here.
		{Idle: 5, WantBase: 8 * base},       // Test 5: And it never grows past the ceiling.
		{Idle: 1 << 20, WantBase: 8 * base}, // Test 6: Nor for an absurd idle count.
	}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			low, high := test.WantBase/2, test.WantBase+test.WantBase/2
			for i := 0; i < 200; i++ {
				got := d.idleWait(test.Idle)
				if got < low || got >= high {
					t.Fatalf("idleWait(%d) = %v, want within [%v, %v)", test.Idle, got, low, high)
				}
			}
		})
	}
}

// TestIdleWaitJitterSpreadsPolls proves the jitter is real rather than a constant offset. Several
// dispatchers sharing one store must not settle into lockstep polling, which is what the random
// share of the wait exists to prevent.
func TestIdleWaitJitterSpreadsPolls(t *testing.T) {
	t.Parallel()
	d := &Dispatcher{claimInterval: time.Second}
	seen := make(map[time.Duration]struct{})
	for i := 0; i < 100; i++ {
		seen[d.idleWait(3)] = struct{}{}
	}
	if len(seen) < 10 {
		t.Errorf("only %d distinct waits over 100 draws, so idle dispatchers poll in step", len(seen))
	}
}

// TestCancelReportsWhetherARunWasCancelable pins the register Cancel reads. The API answers a cancel
// request from this boolean, so a stale entry would report a stopped run that is still changing
// hosts, and a missing one would report nothing to cancel while the run carries on.
func TestCancelReportsWhetherARunWasCancelable(t *testing.T) {
	t.Parallel()
	d := &Dispatcher{cancels: make(map[string]context.CancelFunc)}

	if d.Cancel("run_absent") {
		t.Error("Cancel reported it stopped a run that was never registered")
	}

	ctx, cancel := context.WithCancel(context.Background())
	d.register("run_live", cancel)
	if !d.Cancel("run_live") {
		t.Fatal("Cancel reported nothing to stop for a registered run")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Error("the registered run's context was never canceled")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Errorf("ctx.Err() = %v, want context.Canceled", ctx.Err())
	}

	d.unregister("run_live")
	if d.Cancel("run_live") {
		t.Error("Cancel still reports a run that is no longer cancelable, so the API answers " +
			"canceling for a run nothing will stop")
	}
}

// TestCancelRegisterIsSafeUnderConcurrentUse runs the register the way the dispatcher does: many
// workers registering and unregistering while cancels arrive from the API. It exists for the race
// detector, since the map is guarded by one mutex shared by every executing run.
func TestCancelRegisterIsSafeUnderConcurrentUse(t *testing.T) {
	t.Parallel()
	d := &Dispatcher{cancels: make(map[string]context.CancelFunc)}

	const workers = 16
	var wg sync.WaitGroup
	wg.Add(workers * 2)
	for i := range workers {
		go func(i int) {
			defer wg.Done()
			for j := range 200 {
				id := fmt.Sprintf("run_%d_%d", i, j)
				_, cancel := context.WithCancel(context.Background())
				d.register(id, cancel)
				d.Cancel(id)
				d.unregister(id)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			for j := range 200 {
				d.Cancel(fmt.Sprintf("run_%d_%d", i, j))
			}
		}(i)
	}
	wg.Wait()
	if len(d.cancels) != 0 {
		t.Errorf("%d cancel funcs left registered, so finished runs are still reported cancelable",
			len(d.cancels))
	}
}
