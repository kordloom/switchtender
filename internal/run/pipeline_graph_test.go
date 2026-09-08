package run

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestValidatePipelineGraphRefusalsAreDistinguishable pins each graph refusal against its own
// sentinel error, and that the message names the step at fault.
//
// The dispatcher and the workflow template layer both validate through this one function, so the
// error a caller sees when saving a template is the same one that would have stopped the run. A
// refusal that came back as a generic error would leave somebody editing a fifty-step workflow with
// nothing to look at.
func TestValidatePipelineGraphRefusalsAreDistinguishable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Steps    []PipelineStep
		Want     error
		WantText string
	}{{ // Test 0: A step depending on itself is a cycle of one, which Kahn's algorithm would also
		// catch, but naming it directly says which step is wrong.
		Name: "self dependency",
		Steps: []PipelineStep{
			{Name: "a", Playbook: "a.yml", DependsOn: []string{"a"}},
		},
		Want: ErrDependencyCycle, WantText: "a",
	}, { // Test 1: Two steps sharing a name make a dependency ambiguous.
		Name: "duplicate names",
		Steps: []PipelineStep{
			{Name: "a", Playbook: "a.yml"},
			{Name: "a", Playbook: "b.yml"},
			{Name: "c", Playbook: "c.yml", DependsOn: []string{"a"}},
		},
		Want: ErrDuplicateStep, WantText: "a",
	}, { // Test 2: A dependency naming nothing is refused, with the missing name quoted.
		Name: "unknown dependency",
		Steps: []PipelineStep{
			{Name: "a", Playbook: "a.yml", DependsOn: []string{"ghost"}},
		},
		Want: ErrUnknownDependency, WantText: "ghost",
	}, { // Test 3: A three-step cycle is caught even though no step names itself.
		Name: "longer cycle",
		Steps: []PipelineStep{
			{Name: "a", Playbook: "a.yml", DependsOn: []string{"c"}},
			{Name: "b", Playbook: "b.yml", DependsOn: []string{"a"}},
			{Name: "c", Playbook: "c.yml", DependsOn: []string{"b"}},
		},
		Want: ErrDependencyCycle,
	}, { // Test 4: A cycle off to one side is caught even though part of the graph does sort.
		Name: "cycle beside a valid chain",
		Steps: []PipelineStep{
			{Name: "start", Playbook: "a.yml"},
			{Name: "x", Playbook: "x.yml", DependsOn: []string{"y"}},
			{Name: "y", Playbook: "y.yml", DependsOn: []string{"x"}},
		},
		Want: ErrDependencyCycle,
	}, { // Test 5: Once any step declares a dependency, every step needs a name, including the ones
		// that declare none.
		Name: "an unnamed step beside a graph",
		Steps: []PipelineStep{
			{Playbook: "a.yml"},
			{Name: "b", Playbook: "b.yml", DependsOn: []string{"b"}},
		},
		Want: ErrUnnamedStep,
	}, { // Test 6: A step depending on the same name twice is not a cycle and not a duplicate.
		Name: "repeated dependency on one step",
		Steps: []PipelineStep{
			{Name: "a", Playbook: "a.yml"},
			{Name: "b", Playbook: "b.yml", DependsOn: []string{"a", "a"}},
		},
		Want: nil,
	}, { // Test 7: A step may depend on one later in the list, since the order is the graph rather
		// than the slice.
		Name: "backward dependency",
		Steps: []PipelineStep{
			{Name: "b", Playbook: "b.yml", DependsOn: []string{"a"}},
			{Name: "a", Playbook: "a.yml"},
		},
		Want: nil,
	}, { // Test 8: A fan-in from many steps onto one is legal.
		Name: "fan in",
		Steps: []PipelineStep{
			{Name: "a", Playbook: "a.yml"},
			{Name: "b", Playbook: "b.yml"},
			{Name: "c", Playbook: "c.yml"},
			{Name: "d", Playbook: "d.yml", DependsOn: []string{"a", "b", "c"}},
		},
		Want: nil,
	}, { // Test 9: An empty dependency name is refused, since no step can carry it.
		Name: "empty dependency name",
		Steps: []PipelineStep{
			{Name: "a", Playbook: "a.yml", DependsOn: []string{""}},
		},
		Want: ErrUnknownDependency,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			err := ValidatePipeline(test.Steps)
			if !errors.Is(err, test.Want) {
				t.Fatalf("ValidatePipeline() error = %v, want %v", err, test.Want)
			}
			if test.WantText != "" && !strings.Contains(fmt.Sprint(err), test.WantText) {
				t.Errorf("the refusal %q does not name %q, so an author cannot find the step",
					err, test.WantText)
			}
		})
	}
}

// TestValidateStepInputMatchesTheSingleRunRule pins that each step must carry the input its tool
// needs, which is checked whether or not the pipeline declares dependencies.
//
// A step with no input reaches the executor and either runs nothing or runs the wrong thing. It is
// the same rule a single run is held to, stated once so the dispatcher and a saved template cannot
// drift into disagreeing about which pipelines are legal.
func TestValidateStepInputMatchesTheSingleRunRule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Step PipelineStep
		Want error
	}{
		{Name: "ansible with a playbook", Step: PipelineStep{Playbook: "a.yml"}}, // Test 0: Fine.
		{Name: "explicit ansible with a playbook",
			Step: PipelineStep{Tool: ToolAnsible, Playbook: "a.yml"}}, // Test 1: Fine.
		{Name: "ansible with no playbook", Step: PipelineStep{}, Want: ErrStepInput}, // Test 2.
		{Name: "ansible with only a command",
			Step: PipelineStep{Command: "echo hi"}, Want: ErrStepInput}, // Test 3: A command is not
		// a playbook, so this step would run nothing.
		{Name: "bash with a command", Step: PipelineStep{Tool: ToolBash, Command: "echo"}}, // Test 4.
		{Name: "bash with no command",
			Step: PipelineStep{Tool: ToolBash}, Want: ErrStepInput}, // Test 5.
		{Name: "bash with only a playbook",
			Step: PipelineStep{Tool: ToolBash, Playbook: "a.yml"}, Want: ErrStepInput}, // Test 6.
		{Name: "terraform with a directory",
			Step: PipelineStep{Tool: ToolTerraform, Command: "/infra"}}, // Test 7.
		{Name: "opentofu with no directory",
			Step: PipelineStep{Tool: ToolOpenTofu}, Want: ErrStepInput}, // Test 8.
		{Name: "python with a script",
			Step: PipelineStep{Tool: ToolPython, Command: "print(1)"}}, // Test 9.
		{Name: "powershell with a script",
			Step: PipelineStep{Tool: ToolPowerShell, Command: "Get-Date"}}, // Test 10.
		{Name: "go with source", Step: PipelineStep{Tool: ToolGo, Command: "package main"}}, // Test 11.
		{Name: "unknown tool",
			Step: PipelineStep{Tool: "wat", Command: "x"}, Want: ErrStepInput}, // Test 12.
		{Name: "tool with a stray space",
			Step: PipelineStep{Tool: "bash ", Command: "x"}, Want: ErrStepInput}, // Test 13: The
		// tool name is matched exactly, so a trailing space names nothing.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if err := ValidatePipeline([]PipelineStep{test.Step}); !errors.Is(err, test.Want) {
				t.Errorf("ValidatePipeline(%+v) error = %v, want %v", test.Step, err, test.Want)
			}
		})
	}
}

// TestValidateStepInputLabelsAnUnnamedStepByPosition pins that a refusal for a step with no name
// says which step it is, since a linear pipeline need not name its steps and a message reading only
// "needs a playbook" would leave an author guessing.
func TestValidateStepInputLabelsAnUnnamedStepByPosition(t *testing.T) {
	t.Parallel()
	err := ValidatePipeline([]PipelineStep{
		{Playbook: "a.yml"}, {Playbook: "b.yml"}, {Tool: ToolBash},
	})
	if !errors.Is(err, ErrStepInput) {
		t.Fatalf("ValidatePipeline() error = %v, want ErrStepInput", err)
	}
	if !strings.Contains(err.Error(), "step 3") {
		t.Errorf("the refusal %q does not say which step is wrong", err)
	}

	// A named step is named instead of numbered.
	err = ValidatePipeline([]PipelineStep{{Name: "deploy", Tool: ToolBash}})
	if !strings.Contains(err.Error(), "deploy") {
		t.Errorf("the refusal %q does not use the step's own name", err)
	}
}

// TestPipelineStepCountBounds pins both edges of the step-count limit.
//
// The dependency closure a graph is validated and scheduled through is quadratic in the step count,
// so an unbounded pipeline is a way to make one request cost a great deal of work. Refusing at the
// boundary and accepting one below it is what makes the limit a limit rather than a suggestion.
func TestPipelineStepCountBounds(t *testing.T) {
	t.Parallel()
	steps := func(n int) []PipelineStep {
		out := make([]PipelineStep, n)
		for i := range out {
			out[i] = PipelineStep{Name: fmt.Sprintf("s%d", i), Tool: ToolBash, Command: "echo hi"}
		}
		return out
	}
	tests := []struct {
		Name  string
		Steps []PipelineStep
		Want  error
	}{
		{Name: "none", Steps: nil, Want: ErrNoSteps},                      // Test 0: Nothing.
		{Name: "empty slice", Steps: []PipelineStep{}, Want: ErrNoSteps},  // Test 1: Same.
		{Name: "one", Steps: steps(1)},                                    // Test 2: The floor.
		{Name: "one below the limit", Steps: steps(MaxPipelineSteps - 1)}, // Test 3.
		{Name: "exactly the limit", Steps: steps(MaxPipelineSteps)},       // Test 4: Accepted.
		{Name: "one past the limit", Steps: steps(MaxPipelineSteps + 1),
			Want: ErrTooManySteps}, // Test 5: Refused.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if err := ValidatePipeline(test.Steps); !errors.Is(err, test.Want) {
				t.Errorf("ValidatePipeline(%d steps) error = %v, want %v",
					len(test.Steps), err, test.Want)
			}
		})
	}
}

// TestALongValidChainIsAccepted pins that a legal pipeline at the limit, wired as a dependency
// chain, is validated rather than mistaken for a cycle.
//
// The cycle test is a topological sort that reports a cycle whenever the order it produced does not
// cover every step, so an ordering bug shows up as a legal workflow being refused, which is a
// refusal an author cannot act on.
func TestALongValidChainIsAccepted(t *testing.T) {
	t.Parallel()
	steps := make([]PipelineStep, MaxPipelineSteps)
	for i := range steps {
		steps[i] = PipelineStep{Name: fmt.Sprintf("s%d", i), Playbook: "a.yml"}
		if i > 0 {
			steps[i].DependsOn = []string{fmt.Sprintf("s%d", i-1)}
		}
	}
	if err := ValidatePipeline(steps); err != nil {
		t.Errorf("a legal chain of %d steps was refused: %v", len(steps), err)
	}

	// Reversing the slice leaves the same graph, so it must still be accepted.
	for i, j := 0, len(steps)-1; i < j; i, j = i+1, j-1 {
		steps[i], steps[j] = steps[j], steps[i]
	}
	if err := ValidatePipeline(steps); err != nil {
		t.Errorf("the same graph written in reverse was refused: %v", err)
	}
}
