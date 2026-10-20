package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// testFloor is the floor the free space tests set, 256 MiB.
const testFloor = 256 << 20

// testFree reports the free bytes the test holds, or err when it is set.
type testFree struct {
	// free is the bytes reported.
	free atomic.Uint64
	// calls counts the reads.
	calls atomic.Int64
	// err fails every read when set.
	err error
}

// read is the dispatcher's free space reader.
func (f *testFree) read(string) (uint64, error) {
	f.calls.Add(1)
	if f.err != nil {
		return 0, f.err
	}
	return f.free.Load(), nil
}

// newFloorDispatcher returns a dispatcher with a 256 MiB floor over /data read through f.
func newFloorDispatcher(t *testing.T, store run.Store, f *testFree) *Dispatcher {
	t.Helper()
	runner := &flakyRunnerLister{hosts: []string{"a", "b", "c", "d"}, failHost: "none"}
	d := New(store, runner, zap.NewNop(), WithNoJanitor(), WithFreeSpaceFloor(testFloor, "/data"),
		func(c *config) { c.freeBytes = f.read })
	t.Cleanup(d.Close)
	return d
}

// TestFloorRefusesNewRuns pins the floor on every path that admits a run: one byte under it refuses
// a run, a split and a pipeline with ErrLowSpace naming the path, the space free and the floor, and
// at the floor each is admitted.
func TestFloorRefusesNewRuns(t *testing.T) {
	t.Parallel()
	submits := map[string]func(d *Dispatcher) (*run.Run, error){
		"run": func(d *Dispatcher) (*run.Run, error) {
			return d.Submit(context.Background(), "play.yml", "inv")
		},
		"split": func(d *Dispatcher) (*run.Run, error) {
			return d.SubmitSplit(context.Background(), "play.yml", "inv", 2)
		},
		"pipeline": func(d *Dispatcher) (*run.Run, error) {
			return d.SubmitPipeline(context.Background(), "deploy", "inv",
				[]run.PipelineStep{{Name: "s1", Playbook: "step.yml"}})
		},
	}
	tests := []struct {
		Kind    string
		Free    uint64
		WantErr bool
	}{{ // Test 0: A run one byte under the floor.
		Kind: "run", Free: testFloor - 1, WantErr: true,
	}, { // Test 1: A run at the floor.
		Kind: "run", Free: testFloor,
	}, { // Test 2: A split one byte under the floor.
		Kind: "split", Free: testFloor - 1, WantErr: true,
	}, { // Test 3: A split at the floor.
		Kind: "split", Free: testFloor,
	}, { // Test 4: A pipeline one byte under the floor.
		Kind: "pipeline", Free: testFloor - 1, WantErr: true,
	}, { // Test 5: A pipeline at the floor.
		Kind: "pipeline", Free: testFloor,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f := &testFree{}
			f.free.Store(test.Free)
			store := run.NewMemStore()
			d := newFloorDispatcher(t, store, f)
			r, err := submits[test.Kind](d)
			if !test.WantErr {
				if err != nil {
					t.Fatalf("%s at the floor: error = %v, want admitted", test.Kind, err)
				}
				waitTerminal(t, store, r.ID)
				return
			}
			if !errors.Is(err, ErrLowSpace) {
				t.Fatalf("%s under the floor: error = %v, want ErrLowSpace", test.Kind, err)
			}
			for _, want := range []string{"only 255 MiB free", "/data", "256 MiB floor"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
			if runs, _ := store.List(context.Background()); len(runs) != 0 {
				t.Errorf("a refused %s stored %d runs, want none", test.Kind, len(runs))
			}
		})
	}
}

// TestFloorReturnsAnAdmittedRunOnRetry pins that a retried submit of a run already admitted returns
// that run under the floor, since it asks for no new run, while a new key is refused.
func TestFloorReturnsAnAdmittedRunOnRetry(t *testing.T) {
	t.Parallel()
	f := &testFree{}
	f.free.Store(testFloor)
	store := run.NewMemStore()
	d := newFloorDispatcher(t, store, f)
	ctx := context.Background()
	first, err := d.Submit(ctx, "play.yml", "inv", run.WithIdempotencyKey("idem_1"))
	if err != nil {
		t.Fatalf("Submit(first) error = %v", err)
	}
	f.free.Store(0)
	again, err := d.Submit(ctx, "play.yml", "inv", run.WithIdempotencyKey("idem_1"))
	if err != nil {
		t.Fatalf("Submit(retry) under the floor error = %v, want the admitted run", err)
	}
	if again.ID != first.ID {
		t.Errorf("retry id = %q, want %q", again.ID, first.ID)
	}
	if _, err := d.Submit(ctx, "play.yml", "inv", run.WithIdempotencyKey("idem_2")); !errors.Is(err,
		ErrLowSpace) {
		t.Errorf("Submit(new key) under the floor error = %v, want ErrLowSpace", err)
	}
	waitTerminal(t, store, first.ID)
}

// TestFloorRefusesShardRetry pins the floor on a retry of failed shards: under it the retry is
// refused with ErrLowSpace and stores nothing, at it the retry runs, and a retry already made is
// returned under the floor, since it stores nothing new.
func TestFloorRefusesShardRetry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Free        uint64
		Repeat      bool
		Want        error
		WantNewRuns int
	}{{ // Test 0: A retry one byte under the floor.
		Free: testFloor - 1, Want: ErrLowSpace,
	}, { // Test 1: A retry at the floor stores a split and its one shard.
		Free: testFloor, WantNewRuns: 2,
	}, { // Test 2: A retry already made is returned under the floor.
		Free: 0, Repeat: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := &testFree{}
			f.free.Store(testFloor)
			store := run.NewMemStore()
			runner := &flakyRunnerLister{hosts: []string{"a", "b", "c", "d"}, failHost: "b"}
			d := New(store, runner, zap.NewNop(), WithNoJanitor(),
				WithFreeSpaceFloor(testFloor, "/data"), func(c *config) { c.freeBytes = f.read })
			t.Cleanup(d.Close)
			parent, err := d.SubmitSplit(ctx, "play.yml", "inv", 2)
			if err != nil {
				t.Fatalf("SubmitSplit() error = %v", err)
			}
			if got := waitTerminal(t, store, parent.ID); got.Status != run.StatusFailed {
				t.Fatalf("parent status = %q, want failed", got.Status)
			}
			runner.fixed.Store(true)
			var first *run.Run
			if test.Repeat {
				if first, err = d.RetryFailedShards(ctx, parent.ID); err != nil {
					t.Fatalf("RetryFailedShards(first) error = %v", err)
				}
				waitTerminal(t, store, first.ID)
			}
			before := storedRuns(t, store)
			f.free.Store(test.Free)
			retry, err := d.RetryFailedShards(ctx, parent.ID)
			if !errors.Is(err, test.Want) {
				t.Fatalf("RetryFailedShards() error = %v, want %v", err, test.Want)
			}
			if first != nil {
				if diff := cmp.Diff(first.ID, retry.ID); diff != "" {
					t.Errorf("repeated retry id mismatch (-want +got):\n%s", diff)
				}
			}
			if retry != nil && first == nil {
				if got := waitTerminal(t, store, retry.ID); got.Status != run.StatusSucceeded {
					t.Errorf("retry status = %q, want succeeded", got.Status)
				}
			}
			if diff := cmp.Diff(test.WantNewRuns, storedRuns(t, store)-before); diff != "" {
				t.Errorf("new runs stored mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// storedRuns counts every run in store, the shards of each top-level run included, since List
// leaves shards out.
func storedRuns(t *testing.T, store run.Store) int {
	t.Helper()
	ctx := context.Background()
	runs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	n := len(runs)
	for _, r := range runs {
		shards, err := store.Shards(ctx, r.ID)
		if err != nil {
			t.Fatalf("Shards(%s) error = %v", r.ID, err)
		}
		n += len(shards)
	}
	return n
}

// TestFloorAdmitsWhenSpaceCannotBeRead pins that a failed read admits: a transient error reading
// the filesystem is not taken for a full disk.
func TestFloorAdmitsWhenSpaceCannotBeRead(t *testing.T) {
	t.Parallel()
	f := &testFree{err: errors.New("statfs /data: input/output error")}
	store := run.NewMemStore()
	d := newFloorDispatcher(t, store, f)
	r, err := d.Submit(context.Background(), "play.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() with an unreadable floor error = %v, want admitted", err)
	}
	if f.calls.Load() == 0 {
		t.Error("the floor was never read")
	}
	waitTerminal(t, store, r.ID)
}

// TestPipelineAdmittedIsNotStoppedByTheFloor pins that the floor is checked once, when the pipeline
// is admitted: its later steps start and it succeeds after the disk drops under the floor.
func TestPipelineAdmittedIsNotStoppedByTheFloor(t *testing.T) {
	t.Parallel()
	f := &testFree{}
	f.free.Store(testFloor)
	store := run.NewMemStore()
	d := newFloorDispatcher(t, store, f)
	steps := []run.PipelineStep{
		{Name: "s1", Playbook: "one.yml"},
		{Name: "s2", Playbook: "two.yml"},
	}
	parent, err := d.SubmitPipeline(context.Background(), "deploy", "inv", steps)
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	f.free.Store(0)
	if got := waitTerminal(t, store, parent.ID); got.Status != run.StatusSucceeded {
		t.Errorf("pipeline = %s (%s), want succeeded: an admitted pipeline's steps are not refused",
			got.Status, got.Error)
	}
}

// TestHealthReportsTheFloor pins readiness: degraded with the floor's reason while the disk is under
// it, and clean at it.
func TestHealthReportsTheFloor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Free         uint64
		WantDegraded bool
	}{{ // Test 0: Under the floor.
		Free: testFloor - 1, WantDegraded: true,
	}, { // Test 1: At the floor.
		Free: testFloor,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f := &testFree{}
			f.free.Store(test.Free)
			h := newFloorDispatcher(t, run.NewMemStore(), f).Health()
			if h.Degraded != test.WantDegraded {
				t.Fatalf("Health() = %+v, want degraded %v", h, test.WantDegraded)
			}
			if test.WantDegraded && (len(h.Reasons) != 1 ||
				!strings.Contains(h.Reasons[0], "under the 256 MiB floor: new runs are refused")) {
				t.Errorf("Health().Reasons = %q, want the floor named", h.Reasons)
			}
		})
	}
}

// TestNoFloorReadsNothing pins that a zero floor turns the floor off: the dispatcher never reads
// the filesystem, though it was given a path.
func TestNoFloorReadsNothing(t *testing.T) {
	t.Parallel()
	f := &testFree{}
	store := run.NewMemStore()
	d := New(store, okRunner(), zap.NewNop(), WithNoJanitor(), WithFreeSpaceFloor(0, "/data"),
		func(c *config) { c.freeBytes = f.read })
	t.Cleanup(d.Close)
	r, err := d.Submit(context.Background(), "play.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if h := d.Health(); h.Degraded {
		t.Errorf("Health() = %+v, want clean", h)
	}
	if n := f.calls.Load(); n != 0 {
		t.Errorf("free space read %d times with no floor, want 0", n)
	}
	waitTerminal(t, store, r.ID)
}
