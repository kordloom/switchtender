package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/dcadolph/yardmaster/internal/roundhouse"
	"github.com/dcadolph/yardmaster/internal/run"
)

func TestValidateDAG(t *testing.T) {
	t.Parallel()
	step := func(name string, deps ...string) run.PipelineStep {
		return run.PipelineStep{Name: name, Playbook: name + ".yml", DependsOn: deps}
	}
	tests := []struct {
		Steps []run.PipelineStep
		Want  error
	}{
		{ // Test 0: A valid diamond passes.
			Steps: []run.PipelineStep{
				step("a"), step("b", "a"), step("c", "a"), step("d", "b", "c"),
			},
			Want: nil,
		},
		{ // Test 1: A step without a name is rejected.
			Steps: []run.PipelineStep{
				{Playbook: "x.yml"}, step("b", "a"),
			},
			Want: ErrUnnamedStep,
		},
		{ // Test 2: Duplicate names are rejected.
			Steps: []run.PipelineStep{step("a"), step("a"), step("b", "a")},
			Want:  ErrDuplicateStep,
		},
		{ // Test 3: A dependency on an unknown step is rejected.
			Steps: []run.PipelineStep{step("a"), step("b", "ghost")},
			Want:  ErrUnknownDependency,
		},
		{ // Test 4: A self dependency is a cycle.
			Steps: []run.PipelineStep{step("a", "a")},
			Want:  ErrDependencyCycle,
		},
		{ // Test 5: A longer cycle is rejected.
			Steps: []run.PipelineStep{step("a", "c"), step("b", "a"), step("c", "b")},
			Want:  ErrDependencyCycle,
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := validateDAG(test.Steps)
			if !errors.Is(err, test.Want) {
				t.Errorf("validateDAG() error = %v, want %v", err, test.Want)
			}
		})
	}
}

// diamond returns a four step diamond: a, then b and c in parallel, then d.
func diamond(continueOnB bool) []run.PipelineStep {
	return []run.PipelineStep{
		{Name: "a", Playbook: "a.yml"},
		{Name: "b", Playbook: "b.yml", DependsOn: []string{"a"}, ContinueOnFailure: continueOnB},
		{Name: "c", Playbook: "c.yml", DependsOn: []string{"a"}},
		{Name: "d", Playbook: "d.yml", DependsOn: []string{"b", "c"}},
	}
}

// stepNames returns the step names of the stored pipeline children in step order.
func stepNames(t *testing.T, store run.Store, parentID string) []string {
	t.Helper()
	steps, err := store.Steps(context.Background(), parentID)
	if err != nil {
		t.Fatalf("Steps() error = %v", err)
	}
	names := make([]string, len(steps))
	for i, s := range steps {
		names[i] = s.StepName
	}
	return names
}

func TestDispatcherPipelineDAGSkipsAfterFailure(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	d := New(store, &scriptedRunner{failOn: "b.yml"}, nil)
	defer d.Close()

	parent, err := d.SubmitPipeline(context.Background(), "deploy", "inv", diamond(false))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	got := waitTerminal(t, store, parent.ID)
	if got.Status != run.StatusFailed {
		t.Errorf("parent status = %q, want failed", got.Status)
	}

	names := stepNames(t, store, parent.ID)
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		seen[n] = true
	}
	if !seen["a"] || !seen["b"] || !seen["c"] {
		t.Errorf("steps ran = %v, want a, b, and c", names)
	}
	if seen["d"] {
		t.Errorf("step d ran despite its dependency failing, steps = %v", names)
	}
}

func TestDispatcherPipelineDAGContinueOnFailure(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	d := New(store, &scriptedRunner{failOn: "b.yml"}, nil)
	defer d.Close()

	parent, err := d.SubmitPipeline(context.Background(), "deploy", "inv", diamond(true))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	got := waitTerminal(t, store, parent.ID)
	if got.Status != run.StatusFailed {
		t.Errorf("parent status = %q, want failed since b failed", got.Status)
	}

	names := stepNames(t, store, parent.ID)
	if len(names) != 4 {
		t.Fatalf("steps ran = %v, want all four since b continues on failure", names)
	}
	steps, err := store.Steps(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Steps() error = %v", err)
	}
	for _, s := range steps {
		if s.StepName == "d" && s.Status != run.StatusSucceeded {
			t.Errorf("step d status = %q, want succeeded", s.Status)
		}
	}
}

// rendezvousRunner succeeds every step and blocks the named pair until both have started, which
// proves independent steps run concurrently.
type rendezvousRunner struct {
	// pair names the two playbooks that must overlap.
	pair [2]string
	// mu guards arrived.
	mu sync.Mutex
	// arrived signals each pair member's arrival by closing its channel.
	arrived map[string]chan struct{}
}

// newRendezvousRunner returns a rendezvousRunner for the two playbooks.
func newRendezvousRunner(first, second string) *rendezvousRunner {
	return &rendezvousRunner{
		pair: [2]string{first, second},
		arrived: map[string]chan struct{}{
			first:  make(chan struct{}),
			second: make(chan struct{}),
		},
	}
}

// Run marks the playbook's arrival and, for the pair, waits until the other member has arrived.
func (r *rendezvousRunner) Run(ctx context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
	other := ""
	switch spec.Playbook {
	case r.pair[0]:
		other = r.pair[1]
	case r.pair[1]:
		other = r.pair[0]
	default:
		return roundhouse.Result{ExitCode: 0}, nil
	}

	r.mu.Lock()
	ch := r.arrived[spec.Playbook]
	select {
	case <-ch:
	default:
		close(ch)
	}
	otherCh := r.arrived[other]
	r.mu.Unlock()

	select {
	case <-otherCh:
		return roundhouse.Result{ExitCode: 0}, nil
	case <-time.After(2 * time.Second):
		return roundhouse.Result{ExitCode: 1}, errors.New("pair never overlapped")
	case <-ctx.Done():
		return roundhouse.Result{ExitCode: -1}, ctx.Err()
	}
}

func TestDispatcherPipelineDAGRunsBranchesInParallel(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	d := New(store, newRendezvousRunner("b.yml", "c.yml"), nil)
	defer d.Close()

	parent, err := d.SubmitPipeline(context.Background(), "deploy", "inv", diamond(false))
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	got := waitTerminal(t, store, parent.ID)
	if got.Status != run.StatusSucceeded {
		t.Errorf("parent status = %q, want succeeded, so b and c overlapped", got.Status)
	}
	if names := stepNames(t, store, parent.ID); len(names) != 4 {
		t.Errorf("steps ran = %v, want all four", names)
	}
}

// flakyStepRunner fails the named playbook failCount times, then succeeds. Other playbooks always
// succeed.
type flakyStepRunner struct {
	// flakyOn is the playbook that fails at first.
	flakyOn string
	// failCount is how many failures happen before success.
	failCount int
	// calls counts invocations of the flaky playbook.
	calls int
	// mu guards calls.
	mu sync.Mutex
}

// Run fails the flaky playbook until its failure budget is used up.
func (r *flakyStepRunner) Run(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
	if spec.Playbook != r.flakyOn {
		return roundhouse.Result{ExitCode: 0}, nil
	}
	r.mu.Lock()
	r.calls++
	failing := r.calls <= r.failCount
	r.mu.Unlock()
	if failing {
		return roundhouse.Result{ExitCode: 2}, nil
	}
	return roundhouse.Result{ExitCode: 0}, nil
}

func TestDispatcherPipelineStepRetries(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	d := New(store, &flakyStepRunner{flakyOn: "b.yml", failCount: 2}, nil)
	defer d.Close()

	parent, err := d.SubmitPipeline(context.Background(), "deploy", "inv", []run.PipelineStep{
		{Name: "a", Playbook: "a.yml"},
		{Name: "b", Playbook: "b.yml", Retries: 2},
		{Name: "c", Playbook: "c.yml"},
	})
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	got := waitTerminal(t, store, parent.ID)
	if got.Status != run.StatusSucceeded {
		t.Errorf("parent status = %q, want succeeded after retries", got.Status)
	}

	steps, err := store.Steps(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Steps() error = %v", err)
	}
	var bAttempts []run.Status
	for _, s := range steps {
		if s.StepName == "b" {
			if s.Attempt != len(bAttempts) {
				t.Errorf("b attempt order = %d at position %d", s.Attempt, len(bAttempts))
			}
			bAttempts = append(bAttempts, s.Status)
		}
	}
	want := []run.Status{run.StatusFailed, run.StatusFailed, run.StatusSucceeded}
	if len(bAttempts) != 3 || bAttempts[0] != want[0] || bAttempts[1] != want[1] || bAttempts[2] != want[2] {
		t.Errorf("b attempts = %v, want %v", bAttempts, want)
	}
}

func TestDispatcherPipelineStepRetriesExhausted(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	d := New(store, &scriptedRunner{failOn: "b.yml"}, nil)
	defer d.Close()

	parent, err := d.SubmitPipeline(context.Background(), "deploy", "inv", []run.PipelineStep{
		{Name: "a", Playbook: "a.yml"},
		{Name: "b", Playbook: "b.yml", Retries: 1},
		{Name: "c", Playbook: "c.yml"},
	})
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	got := waitTerminal(t, store, parent.ID)
	if got.Status != run.StatusFailed {
		t.Errorf("parent status = %q, want failed once retries are spent", got.Status)
	}

	steps, err := store.Steps(context.Background(), parent.ID)
	if err != nil {
		t.Fatalf("Steps() error = %v", err)
	}
	attempts := 0
	for _, s := range steps {
		if s.StepName == "b" {
			attempts++
		}
		if s.StepName == "c" {
			t.Error("step c ran after b exhausted its retries")
		}
	}
	if attempts != 2 {
		t.Errorf("b ran %d times, want 2, the first try plus one retry", attempts)
	}
}

func TestDispatcherPipelineDAGRetries(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	d := New(store, &flakyStepRunner{flakyOn: "b.yml", failCount: 1}, nil)
	defer d.Close()

	steps := diamond(false)
	steps[1].Retries = 1
	parent, err := d.SubmitPipeline(context.Background(), "deploy", "inv", steps)
	if err != nil {
		t.Fatalf("SubmitPipeline() error = %v", err)
	}
	got := waitTerminal(t, store, parent.ID)
	if got.Status != run.StatusSucceeded {
		t.Errorf("parent status = %q, want succeeded, b recovered on retry so d ran", got.Status)
	}
	names := stepNames(t, store, parent.ID)
	if len(names) != 5 {
		t.Errorf("step runs = %v, want 5, four steps plus one retry attempt", names)
	}
}

func TestDispatcherPipelineDAGValidation(t *testing.T) {
	t.Parallel()
	d := New(run.NewMemStore(), &scriptedRunner{}, nil)
	defer d.Close()

	_, err := d.SubmitPipeline(context.Background(), "deploy", "inv", []run.PipelineStep{
		{Name: "a", Playbook: "a.yml", DependsOn: []string{"ghost"}},
	})
	if !errors.Is(err, ErrUnknownDependency) {
		t.Errorf("SubmitPipeline() error = %v, want ErrUnknownDependency", err)
	}
}
