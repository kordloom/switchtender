package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// stepVarsRunner publishes fixed outputs for the playbook "build.yml" and records the extra vars
// every step ran with, keyed by its playbook file name or, for a step with none, its tool.
type stepVarsRunner struct {
	// publish is what build.yml prints with set_stats.
	publish map[string]any
	// mu guards received.
	mu sync.Mutex
	// received maps a step's playbook file name or tool to the extra vars it ran with.
	received map[string]map[string]any
}

// Run records the step's vars and writes build.yml's stats event.
func (r *stepVarsRunner) Run(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
	// A step that is not Ansible carries the project checkout in Playbook, so it is keyed by tool.
	key := filepath.Base(spec.Playbook)
	if run.NormalizeTool(spec.Tool) != run.ToolAnsible {
		key = spec.Tool
	}
	r.mu.Lock()
	if r.received == nil {
		r.received = map[string]map[string]any{}
	}
	r.received[key] = spec.ExtraVars
	r.mu.Unlock()
	if key == "build.yml" && spec.EventsPath != "" {
		data, err := json.Marshal(map[string]any{"type": "stats", "ts": 1719000000,
			"outputs": r.publish})
		if err != nil {
			return roundhouse.Result{ExitCode: -1}, err
		}
		if err := os.WriteFile(spec.EventsPath, append(data, '\n'), 0o600); err != nil {
			return roundhouse.Result{ExitCode: -1}, err
		}
	}
	return roundhouse.Result{ExitCode: 0}, nil
}

// ran returns the vars a step ran with and whether it ran at all.
func (r *stepVarsRunner) ran(key string) (map[string]any, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	vars, ok := r.received[key]
	return vars, ok
}

// stepOutputShape is how the two steps are joined: one after the other, as a graph, or as a graph
// with an approval between them, so the second step starts on a resumed walk that read the first
// step's outputs back from its stored row.
type stepOutputShape string

// The three entries a step's outputs reach the next step through.
const (
	outputsLinear stepOutputShape = "linear"
	outputsDAG    stepOutputShape = "dag"
	outputsResume stepOutputShape = "resume"
)

// stepOutputSteps returns build then next, joined the way shape says.
func stepOutputSteps(shape stepOutputShape, next run.PipelineStep) []run.PipelineStep {
	build := run.PipelineStep{Name: "build", Playbook: "build.yml"}
	switch shape {
	case outputsDAG:
		next.DependsOn = []string{"build"}
		return []run.PipelineStep{build, next}
	case outputsResume:
		next.DependsOn = []string{"gate"}
		return []run.PipelineStep{build, {Name: "gate", Type: run.StepApproval,
			Description: "Ship it?", DependsOn: []string{"build"}}, next}
	default:
		return []run.PipelineStep{build, next}
	}
}

// TestStepOutputStaysData drives a workflow whose first step publishes values with set_stats into
// a second step, on each of the three entries.
//
// A published value reached an Ansible step as an extra var, which Ansible renders on the
// controller, so a value holding a lookup ran it there, and a key named ansible_ chose how the next
// step connected. The workflow was approved with its own vars and never with these. A bash step
// reads its vars as plain strings and still gets both.
func TestStepOutputStaysData(t *testing.T) {
	t.Parallel()
	lookup := "{{ lookup('pipe','id') }}"
	tests := []struct {
		Publish   map[string]any
		Next      run.PipelineStep
		WantStop  string
		WantVars  map[string]any
		WantNoKey string
	}{{ // Test 0: Template code in a value stops the workflow before the Ansible step.
		Publish:  map[string]any{"version": lookup, "plain": "1.2.3"},
		Next:     run.PipelineStep{Name: "deploy", Playbook: "deploy.yml"},
		WantStop: `step "deploy" cannot take "version" from step "build"`,
	}, { // Test 1: A plain value reaches the Ansible step.
		Publish:  map[string]any{"plain": "1.2.3"},
		Next:     run.PipelineStep{Name: "deploy", Playbook: "deploy.yml"},
		WantVars: map[string]any{"plain": "1.2.3"},
	}, { // Test 2: A bash step gets both values as plain strings.
		Publish:  map[string]any{"version": lookup, "plain": "1.2.3"},
		Next:     run.PipelineStep{Name: "notify", Tool: run.ToolBash, Command: "notify"},
		WantVars: map[string]any{"version": lookup, "plain": "1.2.3"},
	}, { // Test 3: A published ansible_ name stops the workflow.
		Publish:  map[string]any{"ansible_python_interpreter": "/tmp/x"},
		Next:     run.PipelineStep{Name: "deploy", Playbook: "deploy.yml"},
		WantStop: `cannot take "ansible_python_interpreter" from step "build"`,
	}, { // Test 4: Template code nested in a list stops it too.
		Publish:  map[string]any{"hosts": []any{"web1", map[string]any{"name": lookup}}},
		Next:     run.PipelineStep{Name: "deploy", Playbook: "deploy.yml"},
		WantStop: `cannot take "hosts" from step "build"`,
	}}
	for _, shape := range []stepOutputShape{outputsLinear, outputsDAG, outputsResume} {
		for testNum, test := range tests {
			t.Run(fmt.Sprintf("%s test %d", shape, testNum), func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				store := run.NewMemStore()
				audits := audit.NewMemStore()
				runner := &stepVarsRunner{publish: test.Publish}
				// An approval binds what the steps run, so the playbooks come from a project.
				p := dispatchtest.LocalProject(t, map[string]string{
					"build.yml": "- hosts: all\n  tasks: []\n", "deploy.yml": "- hosts: all\n  tasks: []\n"})
				d := New(store, runner, nil, WithNoJanitor(), WithAudits(audits),
					WithProjects(p.Store, p.Syncer))
				t.Cleanup(d.Close)
				parent, err := d.SubmitPipeline(ctx, "release", "inv",
					stepOutputSteps(shape, test.Next), run.WithProject(p.ID))
				if err != nil {
					t.Fatalf("SubmitPipeline() error = %v", err)
				}
				if shape == outputsResume {
					node := waitParked(t, store, parent.ID)
					if _, err := d.DecideStep(ctx, node.ID, StepDecision{Approve: true,
						By: outcome.Decider{Name: "approver", Type: "session"}}); err != nil {
						t.Fatalf("DecideStep() error = %v", err)
					}
				}
				got := dispatchtest.WaitTerminal(t, store, parent.ID)
				nextKey := test.Next.Playbook
				if nextKey == "" {
					nextKey = test.Next.Tool
				}
				vars, ran := runner.ran(nextKey)
				if test.WantStop != "" {
					if got.Status != run.StatusFailed {
						t.Errorf("workflow status = %s, want failed", got.Status)
					}
					if !strings.Contains(got.Error, test.WantStop) {
						t.Errorf("workflow error = %q, want it to say %q", got.Error, test.WantStop)
					}
					if ran {
						t.Errorf("the next step ran with %v, want it never started", vars)
					}
					return
				}
				if got.Status != run.StatusSucceeded {
					t.Fatalf("workflow status = %s (%s), want succeeded", got.Status, got.Error)
				}
				if !ran {
					t.Fatal("the next step never ran")
				}
				for k, want := range test.WantVars {
					if vars[k] != want {
						t.Errorf("next step got %s = %v, want %v (all: %v)", k, vars[k], want, vars)
					}
				}
			})
		}
	}
}

// TestStepOutputRefusalKeepsApprovedVars confirms the check judges only what a step published. A
// workflow approved with an extra var that holds template code still runs, since a person approved
// it, and an approval step takes nothing.
func TestStepOutputRefusalKeepsApprovedVars(t *testing.T) {
	t.Parallel()
	vars := map[string]any{"msg": "{{ greeting }}", "plain": "1"}
	from := map[string]string{"plain": "build"}
	if got := outputRefusal(run.PipelineStep{Name: "deploy"}, vars, from); got != "" {
		t.Errorf("an approved var with template code was refused: %s", got)
	}
	from["msg"] = "build"
	if got := outputRefusal(run.PipelineStep{Name: "gate", Type: run.StepApproval}, vars,
		from); got != "" {
		t.Errorf("an approval step was refused: %s", got)
	}
	if got := outputRefusal(run.PipelineStep{Name: "deploy"}, vars, from); got == "" {
		t.Error("a published value with template code reached an Ansible step")
	}
}
