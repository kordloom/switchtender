package run

import (
	"errors"
	"fmt"
	"testing"
)

// TestValidatePipeline covers the shared validator both the dispatcher and saved workflow templates
// use: the step count bounds, per-step tool input, and the graph checks that apply only once a
// dependency is declared.
func TestValidatePipeline(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name  string
		Steps []PipelineStep
		Want  error
	}{{ // Test 0: A plain sequence needs no names.
		Name:  "linear unnamed",
		Steps: []PipelineStep{{Playbook: "a.yml"}, {Tool: "bash", Command: "echo hi"}},
		Want:  nil,
	}, { // Test 1: No steps at all.
		Name: "empty", Steps: nil, Want: ErrNoSteps,
	}, { // Test 2: An Ansible step with no playbook.
		Name:  "ansible no playbook",
		Steps: []PipelineStep{{Name: "a"}},
		Want:  ErrStepInput,
	}, { // Test 3: A bash step with no command.
		Name:  "bash no command",
		Steps: []PipelineStep{{Name: "a", Tool: "bash"}},
		Want:  ErrStepInput,
	}, { // Test 4: An unknown tool.
		Name:  "unknown tool",
		Steps: []PipelineStep{{Name: "a", Tool: "wat", Command: "x"}},
		Want:  ErrStepInput,
	}, { // Test 5: A valid diamond graph.
		Name: "valid diamond",
		Steps: []PipelineStep{
			{Name: "a", Playbook: "a.yml"},
			{Name: "b", Playbook: "b.yml", DependsOn: []string{"a"}},
			{Name: "c", Playbook: "c.yml", DependsOn: []string{"a"}},
			{Name: "d", Playbook: "d.yml", DependsOn: []string{"b", "c"}},
		},
		Want: nil,
	}, { // Test 6: A graph with an unnamed step is rejected once dependencies exist.
		Name: "graph unnamed step",
		Steps: []PipelineStep{
			{Playbook: "a.yml"}, {Name: "b", Playbook: "b.yml", DependsOn: []string{"a"}},
		},
		Want: ErrUnnamedStep,
	}, { // Test 7: A cycle.
		Name: "cycle",
		Steps: []PipelineStep{
			{Name: "a", Playbook: "a.yml", DependsOn: []string{"b"}},
			{Name: "b", Playbook: "b.yml", DependsOn: []string{"a"}},
		},
		Want: ErrDependencyCycle,
	}, { // Test 8: A dependency on a step that does not exist.
		Name: "unknown dependency",
		Steps: []PipelineStep{
			{Name: "a", Playbook: "a.yml", DependsOn: []string{"ghost"}},
		},
		Want: ErrUnknownDependency,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if err := ValidatePipeline(test.Steps); !errors.Is(err, test.Want) {
				t.Errorf("ValidatePipeline() error = %v, want %v", err, test.Want)
			}
		})
	}
}

// TestValidatePipelineStepCount proves the upper bound is enforced.
func TestValidatePipelineStepCount(t *testing.T) {
	t.Parallel()
	steps := make([]PipelineStep, MaxPipelineSteps+1)
	for i := range steps {
		steps[i] = PipelineStep{Tool: "bash", Command: "echo hi"}
	}
	if err := ValidatePipeline(steps); !errors.Is(err, ErrTooManySteps) {
		t.Errorf("a pipeline of %d steps was accepted, want ErrTooManySteps", len(steps))
	}
	// One under the limit is fine.
	if err := ValidatePipeline(steps[:MaxPipelineSteps]); err != nil {
		t.Errorf("a pipeline at the limit was rejected: %v", err)
	}
}
