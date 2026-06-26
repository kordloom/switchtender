package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dcadolph/yardmaster/internal/event"
	"github.com/dcadolph/yardmaster/internal/roundhouse"
	"github.com/dcadolph/yardmaster/internal/run"
)

// waitTerminal polls the store until the run reaches a terminal state or the deadline passes.
func waitTerminal(t *testing.T, store run.Store, id string) *run.Run {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
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

// intPtr returns a pointer to v for table expectations.
func intPtr(v int) *int { return &v }

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
			WantStatus: run.StatusSucceeded, WantExitCode: intPtr(0), WantOutput: "ran",
		},
		{ // Test 1: Non-zero exit fails with the recorded code.
			Name: "playbook failed", Result: roundhouse.Result{ExitCode: 2},
			WantStatus: run.StatusFailed, WantExitCode: intPtr(2), WantOutput: "ran",
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
		Shards     int
		WantGroups int
	}{
		{Hosts: []string{"a", "b", "c", "d"}, Shards: 2, WantGroups: 2}, // Test 0: Even split.
		{Hosts: []string{"a", "b", "c"}, Shards: 2, WantGroups: 2},      // Test 1: Uneven split.
		{Hosts: []string{"a"}, Shards: 4, WantGroups: 1},                // Test 2: Fewer hosts than shards.
		{Hosts: []string{"a", "b", "c", "d"}, Shards: 3, WantGroups: 3}, // Test 3: Three groups.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			groups := partition(test.Hosts, test.Shards)
			if len(groups) != test.WantGroups {
				t.Errorf("groups = %d, want %d", len(groups), test.WantGroups)
			}
			total := 0
			for _, g := range groups {
				total += len(g)
			}
			if total != len(test.Hosts) {
				t.Errorf("placed %d hosts, want %d", total, len(test.Hosts))
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
