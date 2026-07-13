package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/dcadolph/yardmaster/internal/credential"
	"github.com/dcadolph/yardmaster/internal/event"
	"github.com/dcadolph/yardmaster/internal/roundhouse"
	"github.com/dcadolph/yardmaster/internal/run"
)

// waitTerminal polls the store until the run reaches a terminal state or the deadline passes.
func waitTerminal(t *testing.T, store run.Store, id string) *run.Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r, err := store.Get(context.Background(), id)
		if err == nil && r.Status.Terminal() {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not reach a terminal state", id)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestDispatcherExecute(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		Result       roundhouse.Result
		RunErr       error
		WantStatus   run.Status
		WantExitCode *int
		WantOutput   string
	}{
		{ // Test 0: Zero exit succeeds.
			Name: "success", Result: roundhouse.Result{ExitCode: 0},
			WantStatus: run.StatusSucceeded, WantExitCode: new(0), WantOutput: "ran",
		},
		{ // Test 1: Non-zero exit fails with the recorded code.
			Name: "playbook failed", Result: roundhouse.Result{ExitCode: 2},
			WantStatus: run.StatusFailed, WantExitCode: new(2), WantOutput: "ran",
		},
		{ // Test 2: Launch error fails with no exit code.
			Name: "launch error", RunErr: errors.New("boom"),
			WantStatus: run.StatusFailed, WantExitCode: nil, WantOutput: "ran",
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := run.NewMemStore()
			runner := roundhouse.RunnerFunc(
				func(_ context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
					_, _ = io.WriteString(out, "ran "+spec.Playbook)
					return test.Result, test.RunErr
				},
			)
			d := New(store, runner, nil)
			defer d.Close()

			created, err := d.Submit(context.Background(), "play.yml", "inv")
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}

			got := waitTerminal(t, store, created.ID)
			if got.Status != test.WantStatus {
				t.Errorf("Status = %q, want %q", got.Status, test.WantStatus)
			}
			switch {
			case test.WantExitCode == nil && got.ExitCode != nil:
				t.Errorf("ExitCode = %d, want nil", *got.ExitCode)
			case test.WantExitCode != nil && (got.ExitCode == nil || *got.ExitCode != *test.WantExitCode):
				t.Errorf("ExitCode = %v, want %d", got.ExitCode, *test.WantExitCode)
			}
			if got.StartedAt == nil || got.EndedAt == nil {
				t.Error("StartedAt and EndedAt should be set on a terminal run")
			}
			body, err := store.Log(context.Background(), created.ID)
			if err != nil {
				t.Fatalf("Log() error = %v", err)
			}
			if !strings.Contains(string(body), test.WantOutput) {
				t.Errorf("log %q does not contain %q", body, test.WantOutput)
			}
		})
	}
}

func TestDispatcherStoresEvents(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			line := `{"type":"play_start","ts":1719000000,"play":"demo"}` + "\n"
			if err := os.WriteFile(spec.EventsPath, []byte(line), 0o600); err != nil {
				return roundhouse.Result{ExitCode: -1}, err
			}
			return roundhouse.Result{ExitCode: 0}, nil
		},
	)
	d := New(store, runner, nil)
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	waitTerminal(t, store, created.ID)
	events, err := store.Events(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Events() error = %v", err)
	}
	if len(events) != 1 || events[0].Type != event.TypePlayStart || events[0].Play != "demo" {
		t.Errorf("Events() = %+v, want one play_start for demo", events)
	}
}

func TestDispatcherBashRun(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	var (
		mu      sync.Mutex
		gotSpec roundhouse.Spec
	)
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
			mu.Lock()
			gotSpec = spec
			mu.Unlock()
			_, _ = io.WriteString(out, "bash ran")
			return roundhouse.Result{ExitCode: 0}, nil
		},
	)
	d := New(store, runner, nil)
	defer d.Close()

	created, err := d.Submit(context.Background(), "", "",
		run.WithTool(run.ToolBash), run.WithCommand("echo hi"), run.WithDryRun(true))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	got := waitTerminal(t, store, created.ID)
	if got.Status != run.StatusSucceeded {
		t.Errorf("Status = %q, want succeeded", got.Status)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotSpec.Tool != run.ToolBash || gotSpec.Command != "echo hi" || !gotSpec.DryRun {
		t.Errorf("spec = %+v, want tool=bash command='echo hi' dryRun=true", gotSpec)
	}
}

func TestDispatcherRejectsBashWithoutCommand(t *testing.T) {
	t.Parallel()
	d := New(run.NewMemStore(), roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		}), nil)
	defer d.Close()

	if _, err := d.Submit(context.Background(), "", "", run.WithTool(run.ToolBash)); !errors.Is(err, ErrNoCommand) {
		t.Errorf("Submit() error = %v, want ErrNoCommand", err)
	}
}

func TestDispatcherSubmitNoPlaybook(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{}, nil
		},
	)
	d := New(store, runner, nil)
	defer d.Close()

	if _, err := d.Submit(context.Background(), "", "inv"); !errors.Is(err, ErrNoPlaybook) {
		t.Errorf("Submit() error = %v, want ErrNoPlaybook", err)
	}
}

func TestDispatcherCloseCancelsRun(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	started := make(chan struct{})
	runner := roundhouse.RunnerFunc(
		func(ctx context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			close(started)
			<-ctx.Done()
			return roundhouse.Result{ExitCode: -1}, ctx.Err()
		},
	)
	d := New(store, runner, nil, WithWorkers(1))

	created, err := d.Submit(context.Background(), "play.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	<-started
	d.Close()

	got, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusCanceled {
		t.Errorf("Status = %q, want %q", got.Status, run.StatusCanceled)
	}
}

func TestWaitChildrenReturnsPromptlyOnCancel(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	now := time.Now()
	ids := []string{"run_c1", "run_c2"}
	// Children already leased by another process that never finish, so only cancellation, not a
	// terminal state, can end the wait.
	for _, id := range ids {
		if err := store.Save(context.Background(), &run.Run{
			ID: id, Status: run.StatusRunning, ClaimedBy: "ghost", ClaimedAt: &now, CreatedAt: now,
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	d := New(store, roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{}, nil
		}), nil)
	defer d.Close()

	// The parent context is already canceled, as it would be during shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan []run.Status, 1)
	go func() { done <- d.waitChildren(ctx, ids) }()
	select {
	case statuses := <-done:
		for i, s := range statuses {
			if s != run.StatusCanceled {
				t.Errorf("child %d status = %q, want %q", i, s, run.StatusCanceled)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitChildren did not return promptly after cancellation")
	}
}

// fakeRunnerLister is a Runner that also lists a fixed set of hosts for split tests.
type fakeRunnerLister struct {
	// hosts is returned by Hosts.
	hosts []string
}

// Run reports success without doing anything.
func (f *fakeRunnerLister) Run(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
	return roundhouse.Result{ExitCode: 0}, nil
}

// Hosts returns the fixed host set.
func (f *fakeRunnerLister) Hosts(context.Context, string) ([]string, error) {
	return f.hosts, nil
}

func TestPartition(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Hosts      []string
		Costs      map[string]float64
		Shards     int
		WantGroups [][]string
	}{
		{ // Test 0: No costs balances by host count.
			Hosts: []string{"a", "b", "c", "d"}, Shards: 2,
			WantGroups: [][]string{{"a", "c"}, {"b", "d"}},
		},
		{ // Test 1: Uneven host count without costs.
			Hosts: []string{"a", "b", "c"}, Shards: 2,
			WantGroups: [][]string{{"a", "c"}, {"b"}},
		},
		{ // Test 2: Fewer hosts than shards collapses to one group per host.
			Hosts: []string{"a"}, Shards: 4,
			WantGroups: [][]string{{"a"}},
		},
		{ // Test 3: One expensive host gets its own shard.
			Hosts: []string{"a", "b", "c", "d"}, Shards: 2,
			Costs:      map[string]float64{"a": 10, "b": 1, "c": 1, "d": 1},
			WantGroups: [][]string{{"a"}, {"b", "c", "d"}},
		},
		{ // Test 4: A host without history weighs the average of the known costs.
			Hosts: []string{"a", "b", "c"}, Shards: 2,
			Costs:      map[string]float64{"a": 6, "b": 2},
			WantGroups: [][]string{{"a"}, {"c", "b"}},
		},
		{ // Test 5: Zero recorded cost counts as no history, not as free.
			Hosts: []string{"a", "b", "c", "d"}, Shards: 2,
			Costs:      map[string]float64{"a": 0, "b": 0, "c": 0, "d": 0},
			WantGroups: [][]string{{"a", "c"}, {"b", "d"}},
		},
		{ // Test 6: Costs spread across three shards.
			Hosts: []string{"a", "b", "c", "d", "e", "f"}, Shards: 3,
			Costs:      map[string]float64{"a": 9, "b": 8, "c": 7, "d": 3, "e": 2, "f": 1},
			WantGroups: [][]string{{"a", "f"}, {"b", "e"}, {"c", "d"}},
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := partition(test.Hosts, test.Shards, test.Costs)
			if diff := cmp.Diff(test.WantGroups, got); diff != "" {
				t.Errorf("partition mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDispatcherSubmitSplit(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	d := New(store, &fakeRunnerLister{hosts: []string{"a", "b", "c", "d"}}, nil)
	defer d.Close()

	parent, err := d.SubmitSplit(context.Background(), "play.yml", "inv", 2)
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	if parent.ShardCount == nil || *parent.ShardCount != 2 {
		t.Fatalf("parent ShardCount = %v, want 2", parent.ShardCount)
	}

	got := waitTerminal(t, store, parent.ID)
	if got.Status != run.StatusSucceeded {
		t.Errorf("parent status = %q, want succeeded", got.Status)
	}

	shards, err := store.Shards(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	if len(shards) != 2 {
		t.Fatalf("shards = %d, want 2", len(shards))
	}
	for _, s := range shards {
		if strings.Count(s.Limit, ",") != 1 {
			t.Errorf("shard limit %q should target two hosts", s.Limit)
		}
		if s.Status != run.StatusSucceeded {
			t.Errorf("shard status = %q, want succeeded", s.Status)
		}
	}

	top, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, r := range top {
		if r.ParentID != nil {
			t.Errorf("List returned shard %s", r.ID)
		}
	}
}

func TestDispatcherSplitFallsBackAndErrors(t *testing.T) {
	t.Parallel()

	// A runner that cannot list hosts rejects a split.
	plain := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{}, nil
		},
	)
	d := New(run.NewMemStore(), plain, nil)
	defer d.Close()
	if _, err := d.SubmitSplit(context.Background(), "play.yml", "inv", 2); !errors.Is(err, ErrNoHostLister) {
		t.Errorf("SubmitSplit() error = %v, want ErrNoHostLister", err)
	}

	// One shard falls back to a single non-shard run.
	store := run.NewMemStore()
	d2 := New(store, &fakeRunnerLister{hosts: []string{"a", "b"}}, nil)
	defer d2.Close()
	single, err := d2.SubmitSplit(context.Background(), "play.yml", "inv", 1)
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	if single.ShardCount != nil || single.ParentID != nil {
		t.Errorf("single run should not be a split: %+v", single)
	}
}

// flakyRunnerLister lists fixed hosts and fails any run whose limit includes failHost until fixed
// flips, after which every run succeeds.
type flakyRunnerLister struct {
	// hosts is returned by Hosts.
	hosts []string
	// failHost fails a run whose limit contains it.
	failHost string
	// fixed disables failures once set.
	fixed atomic.Bool
}

// Run fails when the spec limit targets the failing host and the runner is not fixed.
func (f *flakyRunnerLister) Run(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
	if !f.fixed.Load() && strings.Contains(spec.Limit, f.failHost) {
		return roundhouse.Result{ExitCode: 2}, nil
	}
	return roundhouse.Result{ExitCode: 0}, nil
}

// Hosts returns the fixed host set.
func (f *flakyRunnerLister) Hosts(context.Context, string) ([]string, error) {
	return f.hosts, nil
}

// capturingPublisher records published events and closed runs by id for stream assertions.
type capturingPublisher struct {
	// mu guards events and closed.
	mu sync.Mutex
	// events maps a topic id to the events published under it.
	events map[string][]event.Event
	// closed lists the ids whose topics were closed.
	closed []string
}

// newCapturingPublisher returns an empty capturingPublisher.
func newCapturingPublisher() *capturingPublisher {
	return &capturingPublisher{events: make(map[string][]event.Event)}
}

// PublishEvents records events under the topic id.
func (c *capturingPublisher) PublishEvents(id string, events []event.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events[id] = append(c.events[id], events...)
}

// PublishLog discards log chunks.
func (c *capturingPublisher) PublishLog(string, []byte) {}

// CloseRun records the closed topic id.
func (c *capturingPublisher) CloseRun(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = append(c.closed, id)
}

// eventCount returns how many events were published under id.
func (c *capturingPublisher) eventCount(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events[id])
}

// closedIDs returns a copy of the closed topic ids.
func (c *capturingPublisher) closedIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.closed))
	copy(out, c.closed)
	return out
}

// eventWritingLister lists fixed hosts and writes one play_start event line per run.
type eventWritingLister struct {
	// hosts is returned by Hosts.
	hosts []string
}

// Run writes a single event line to the sidecar and succeeds.
func (f *eventWritingLister) Run(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
	if spec.EventsPath != "" {
		line := `{"type":"play_start","ts":1719000000,"play":"demo"}` + "\n"
		if err := os.WriteFile(spec.EventsPath, []byte(line), 0o600); err != nil {
			return roundhouse.Result{ExitCode: -1}, err
		}
	}
	return roundhouse.Result{ExitCode: 0}, nil
}

// Hosts returns the fixed host set.
func (f *eventWritingLister) Hosts(context.Context, string) ([]string, error) {
	return f.hosts, nil
}

func TestDispatcherEchoesChildEventsToParent(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	pub := newCapturingPublisher()
	d := New(store, &eventWritingLister{hosts: []string{"a", "b", "c", "d"}}, nil, WithPublisher(pub))
	defer d.Close()

	parent, err := d.SubmitSplit(context.Background(), "play.yml", "inv", 2)
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	waitTerminal(t, store, parent.ID)

	if got := pub.eventCount(parent.ID); got != 2 {
		t.Errorf("parent topic events = %d, want 2, one echoed from each shard", got)
	}
	closed := pub.closedIDs()
	parentClosed := false
	for _, id := range closed {
		if id == parent.ID {
			parentClosed = true
		}
	}
	if !parentClosed {
		t.Errorf("parent topic was not closed, closed = %v", closed)
	}
}

func TestDispatcherEchoesStepEventsToPipeline(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	pub := newCapturingPublisher()
	d := New(store, &eventWritingLister{}, nil, WithPublisher(pub))
	defer d.Close()

	parent, err := d.SubmitPipeline(context.Background(), "deploy", "inv", []run.PipelineStep{
		{Name: "one", Playbook: "one.yml"},
		{Name: "two", Playbook: "two.yml"},
	})
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	waitTerminal(t, store, parent.ID)

	if got := pub.eventCount(parent.ID); got != 2 {
		t.Errorf("pipeline topic events = %d, want 2, one echoed from each step", got)
	}
}

func TestDispatcherRetryFailedShards(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	runner := &flakyRunnerLister{hosts: []string{"a", "b", "c", "d"}, failHost: "b"}
	d := New(store, runner, nil)
	defer d.Close()

	parent, err := d.SubmitSplit(context.Background(), "play.yml", "inv", 2)
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	if got := waitTerminal(t, store, parent.ID); got.Status != run.StatusFailed {
		t.Fatalf("parent status = %q, want failed", got.Status)
	}

	shards, err := store.Shards(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	var failedLimit string
	for _, s := range shards {
		if s.Status == run.StatusFailed {
			failedLimit = s.Limit
		}
	}
	if !strings.Contains(failedLimit, "b") {
		t.Fatalf("failed shard limit = %q, want the one containing b", failedLimit)
	}

	runner.fixed.Store(true)
	retry, err := d.RetryFailedShards(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("RetryFailedShards() error = %v", err)
	}
	if retry.RetryOf == nil || *retry.RetryOf != parent.ID {
		t.Errorf("RetryOf = %v, want %s", retry.RetryOf, parent.ID)
	}
	if retry.Kind != run.KindSplit {
		t.Errorf("retry kind = %q, want %q", retry.Kind, run.KindSplit)
	}
	if retry.ShardCount == nil || *retry.ShardCount != 1 {
		t.Errorf("retry ShardCount = %v, want 1", retry.ShardCount)
	}

	if got := waitTerminal(t, store, retry.ID); got.Status != run.StatusSucceeded {
		t.Errorf("retry status = %q, want succeeded", got.Status)
	}
	retryShards, err := store.Shards(context.Background(), retry.ID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	if len(retryShards) != 1 || retryShards[0].Limit != failedLimit {
		t.Errorf("retry shards = %+v, want one shard with limit %q", retryShards, failedLimit)
	}

	original, err := store.Get(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if original.Status != run.StatusFailed {
		t.Errorf("original parent status = %q, want failed left untouched", original.Status)
	}
}

func TestDispatcherRetryFailedShardsErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, &fakeRunnerLister{}, nil)
	defer d.Close()

	code := 0
	parentID := "run_ok_split"
	idx, count := 0, 1
	for _, r := range []*run.Run{
		{ID: "run_plain", Status: run.StatusFailed, CreatedAt: time.Now()},
		{ID: "run_live_split", Kind: run.KindSplit, Status: run.StatusRunning, CreatedAt: time.Now()},
		{
			ID: parentID, Kind: run.KindSplit, Status: run.StatusSucceeded,
			ExitCode: &code, CreatedAt: time.Now(),
		},
		{
			ID: "run_ok_shard", Status: run.StatusSucceeded, CreatedAt: time.Now(),
			ParentID: &parentID, ShardIndex: &idx, ShardCount: &count,
		},
	} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}

	tests := []struct {
		ID   string
		Want error
	}{
		{ID: "missing", Want: run.ErrNotFound},       // Test 0: Unknown run.
		{ID: "run_plain", Want: ErrNotSplit},         // Test 1: Not a split parent.
		{ID: "run_live_split", Want: ErrNotFinished}, // Test 2: Split still running.
		{ID: parentID, Want: ErrNoFailedShards},      // Test 3: Every shard succeeded.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if _, err := d.RetryFailedShards(ctx, test.ID); !errors.Is(err, test.Want) {
				t.Errorf("RetryFailedShards() error = %v, want %v", err, test.Want)
			}
		})
	}
}

func TestDispatcherCancel(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	started := make(chan struct{})
	runner := roundhouse.RunnerFunc(
		func(ctx context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			close(started)
			<-ctx.Done()
			return roundhouse.Result{ExitCode: -1}, ctx.Err()
		},
	)
	d := New(store, runner, nil)
	defer d.Close()

	r, err := d.Submit(context.Background(), "play.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	<-started

	if !d.Cancel(r.ID) {
		t.Fatal("Cancel returned false for a running run")
	}
	got := waitTerminal(t, store, r.ID)
	if got.Status != run.StatusCanceled {
		t.Errorf("status = %q, want canceled", got.Status)
	}
	if d.Cancel("nope") {
		t.Error("Cancel returned true for an unknown run")
	}
}

// scriptedRunner succeeds for every playbook except failOn, which exits non-zero.
type scriptedRunner struct {
	// failOn is the playbook that fails.
	failOn string
}

// Run succeeds unless the spec playbook matches failOn.
func (s *scriptedRunner) Run(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
	if spec.Playbook == s.failOn {
		return roundhouse.Result{ExitCode: 2}, nil
	}
	return roundhouse.Result{ExitCode: 0}, nil
}

func TestDispatcherSubmitPipeline(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	d := New(store, &scriptedRunner{}, nil)
	defer d.Close()

	parent, err := d.SubmitPipeline(context.Background(), "deploy", "inv", []run.PipelineStep{
		{Name: "one", Playbook: "one.yml"},
		{Name: "two", Playbook: "two.yml"},
	})
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	if parent.Kind != run.KindPipeline {
		t.Errorf("parent kind = %q, want %q", parent.Kind, run.KindPipeline)
	}

	got := waitTerminal(t, store, parent.ID)
	if got.Status != run.StatusSucceeded {
		t.Errorf("parent status = %q, want succeeded", got.Status)
	}
	steps, err := store.Steps(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Steps() error = %v", err)
	}
	if len(steps) != 2 || steps[0].StepName != "one" || steps[1].StepName != "two" {
		t.Errorf("steps = %+v, want ordered one then two", steps)
	}
}

func TestDispatcherPipelineStopOnFailure(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	d := New(store, &scriptedRunner{failOn: "two.yml"}, nil)
	defer d.Close()

	parent, err := d.SubmitPipeline(context.Background(), "deploy", "inv", []run.PipelineStep{
		{Name: "one", Playbook: "one.yml"},
		{Name: "two", Playbook: "two.yml"},
		{Name: "three", Playbook: "three.yml"},
	})
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	got := waitTerminal(t, store, parent.ID)
	if got.Status != run.StatusFailed {
		t.Errorf("parent status = %q, want failed", got.Status)
	}
	steps, _ := store.Steps(context.Background(), parent.ID)
	if len(steps) != 2 {
		t.Errorf("ran %d steps, want 2 with the third skipped", len(steps))
	}
}

func TestDispatcherPipelineContinueOnFailure(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	d := New(store, &scriptedRunner{failOn: "two.yml"}, nil)
	defer d.Close()

	parent, err := d.SubmitPipeline(context.Background(), "deploy", "inv", []run.PipelineStep{
		{Name: "one", Playbook: "one.yml"},
		{Name: "two", Playbook: "two.yml", ContinueOnFailure: true},
		{Name: "three", Playbook: "three.yml"},
	})
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	got := waitTerminal(t, store, parent.ID)
	if got.Status != run.StatusFailed {
		t.Errorf("parent status = %q, want failed since a step failed", got.Status)
	}
	steps, _ := store.Steps(context.Background(), parent.ID)
	if len(steps) != 3 {
		t.Errorf("ran %d steps, want 3 since the failure continues", len(steps))
	}
}

func TestDispatcherPipelineNoSteps(t *testing.T) {
	t.Parallel()
	d := New(run.NewMemStore(), &scriptedRunner{}, nil)
	defer d.Close()
	if _, err := d.SubmitPipeline(context.Background(), "x", "inv", nil); !errors.Is(err, ErrNoSteps) {
		t.Errorf("SubmitPipeline() error = %v, want ErrNoSteps", err)
	}
}

func TestNewPanicsOnNilDeps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name   string
		Store  run.Store
		Runner roundhouse.Runner
	}{
		{Name: "nil store", Store: nil, Runner: roundhouse.NewAnsibleRunner()}, // Test 0.
		{Name: "nil runner", Store: run.NewMemStore(), Runner: nil},            // Test 1.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Error("New() did not panic on nil dependency")
				}
			}()
			New(test.Store, test.Runner, nil)
		})
	}
}

func TestDispatcherNotifiesWebhook(t *testing.T) {
	t.Parallel()
	received := make(chan string, 4)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n struct {
			Event string `json:"event"`
			Run   struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"run"`
		}
		if err := json.NewDecoder(r.Body).Decode(&n); err == nil {
			received <- n.Event + ":" + n.Run.ID + ":" + n.Run.Status
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hook.Close()

	store := run.NewMemStore()
	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		})
	d := New(store, runner, nil, WithWebhooks([]string{hook.URL}))
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitTerminal(t, store, created.ID)

	select {
	case got := <-received:
		want := "run.finished:" + created.ID + ":succeeded"
		if got != want {
			t.Errorf("notification = %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("webhook never received the notification")
	}
}

// captureEmailer records the subjects it is asked to send.
type captureEmailer struct {
	// sent receives one subject per Send call.
	sent chan string
}

// Send records the subject and reports success.
func (c *captureEmailer) Send(_ context.Context, subject, _ string) error {
	c.sent <- subject
	return nil
}

func TestDispatcherEmailsOnFailure(t *testing.T) {
	t.Parallel()
	emailer := &captureEmailer{sent: make(chan string, 4)}
	store := run.NewMemStore()
	fail := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 2}, nil
		})
	d := New(store, fail, nil, WithEmail(emailer, true))
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitTerminal(t, store, created.ID)

	select {
	case subject := <-emailer.sent:
		if !strings.Contains(subject, created.ID) || !strings.Contains(subject, "failed") {
			t.Errorf("subject = %q, want it to name the failed run", subject)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("email was never sent for a failed run")
	}
}

func TestDispatcherSkipsEmailOnSuccessWhenFailureOnly(t *testing.T) {
	t.Parallel()
	emailer := &captureEmailer{sent: make(chan string, 4)}
	store := run.NewMemStore()
	ok := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		})
	d := New(store, ok, nil, WithEmail(emailer, true))
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv")
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitTerminal(t, store, created.ID)

	select {
	case subject := <-emailer.sent:
		t.Errorf("unexpected email for a succeeded run: %q", subject)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestRunImageReachesSpec verifies a run's execution image and its decrypted registry login land
// on the Spec the runner receives, and a bash run pinning an image is rejected at submit.
func TestRunImageReachesSpec(t *testing.T) {
	t.Parallel()
	sealer := credential.NewSealer("pass", "salt")
	sealed, err := sealer.Seal("bot\nhunter2")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	creds := credential.NewMemStore()
	if err := creds.Save(context.Background(), &credential.Credential{
		ID: "cred_pull", Kind: credential.KindRegistry, Secret: sealed,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got := make(chan roundhouse.Spec, 1)
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			got <- spec
			return roundhouse.Result{ExitCode: 0}, nil
		},
	)
	store := run.NewMemStore()
	d := New(store, runner, nil, WithCredentials(creds, sealer))
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv.ini",
		run.WithImage("ghcr.io/acme/ee:9", "cred_pull"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	waitTerminal(t, store, created.ID)
	spec := <-got
	if spec.Image != "ghcr.io/acme/ee:9" {
		t.Errorf("spec.Image = %q, want ghcr.io/acme/ee:9", spec.Image)
	}
	if spec.RegistryUsername != "bot" || spec.RegistryPassword != "hunter2" {
		t.Errorf("registry login = %q/%q, want bot/hunter2", spec.RegistryUsername, spec.RegistryPassword)
	}

	// Test 1: A non-Ansible tool cannot pin an image.
	_, err = d.Submit(context.Background(), "", "",
		run.WithTool("bash"), run.WithCommand("echo hi"), run.WithImage("ghcr.io/acme/ee:9", ""))
	if !errors.Is(err, ErrImageTool) {
		t.Errorf("Submit(bash+image) error = %v, want ErrImageTool", err)
	}
}
