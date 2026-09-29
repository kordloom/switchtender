package run

import (
	"errors"
	"fmt"
)

// MaxPipelineSteps bounds one pipeline. The dependency closure a graph is validated and scheduled
// through is quadratic in the step count, so an unbounded pipeline is a way to make one request cost
// a lot of work.
const MaxPipelineSteps = 500

// Pipeline validation errors. They live here, with the step type, so the dispatcher that runs a
// pipeline and the template layer that saves one validate it through the same code and cannot drift
// into disagreeing about which pipelines are legal.
var (
	// ErrNoSteps is returned when a pipeline carries no steps.
	ErrNoSteps = errors.New("no steps")
	// ErrTooManySteps is returned when a pipeline exceeds MaxPipelineSteps.
	ErrTooManySteps = errors.New("too many steps")
	// ErrStepInput is returned when a step lacks the input its tool needs, or names an unknown tool.
	ErrStepInput = errors.New("pipeline step input invalid")
	// ErrUnnamedStep is returned when a dependency-declaring pipeline has a step without a name.
	ErrUnnamedStep = errors.New("step missing name")
	// ErrDuplicateStep is returned when two pipeline steps share a name.
	ErrDuplicateStep = errors.New("duplicate step name")
	// ErrUnknownDependency is returned when a step depends on a name no step carries.
	ErrUnknownDependency = errors.New("unknown dependency")
	// ErrDependencyCycle is returned when pipeline dependencies form a cycle.
	ErrDependencyCycle = errors.New("dependency cycle")
)

// ValidatePipeline checks that a pipeline is legal: it has between one and MaxPipelineSteps steps,
// each step carries the input its tool needs, and, when any step declares a dependency, the steps
// are uniquely named and their dependencies form a directed acyclic graph.
//
// The name and graph checks apply only when a dependency is declared, so a plain sequence of steps
// need not name them, which is how a linear pipeline has always been accepted. The per-step input
// check always applies. This is the single definition both the dispatcher and a saved workflow
// template validate through.
func ValidatePipeline(steps []PipelineStep) error {
	if len(steps) == 0 {
		return ErrNoSteps
	}
	if len(steps) > MaxPipelineSteps {
		return fmt.Errorf("%w: %d steps, the limit is %d", ErrTooManySteps, len(steps), MaxPipelineSteps)
	}
	for i, s := range steps {
		if err := validateStepInput(s, i); err != nil {
			return err
		}
	}
	if pipelineHasDependencies(steps) {
		return validatePipelineGraph(steps)
	}
	return nil
}

// validateStepInput checks that one step carries the input its tool needs, mirroring the single-run
// tool-input rule: any tool must be known, an Ansible step needs a playbook, and every other tool
// needs a command.
func validateStepInput(s PipelineStep, idx int) error {
	label := s.Name
	if label == "" {
		label = fmt.Sprintf("step %d", idx+1)
	}
	if !ValidTool(s.Tool) {
		return fmt.Errorf("%w: %s names an unknown tool %q", ErrStepInput, label, s.Tool)
	}
	if NormalizeTool(s.Tool) == ToolAnsible {
		if s.Playbook == "" {
			return fmt.Errorf("%w: %s needs a playbook", ErrStepInput, label)
		}
		return nil
	}
	if s.Command == "" {
		return fmt.Errorf("%w: %s needs a command", ErrStepInput, label)
	}
	return nil
}

// pipelineHasDependencies reports whether any step declares a dependency, which turns the pipeline
// from a sequence into a graph.
func pipelineHasDependencies(steps []PipelineStep) bool {
	for _, s := range steps {
		if len(s.DependsOn) > 0 {
			return true
		}
	}
	return false
}

// validatePipelineGraph checks that a dependency-declaring pipeline has uniquely named steps, that
// every dependency references a known step, and that the graph has no cycles.
func validatePipelineGraph(steps []PipelineStep) error {
	idx := make(map[string]int, len(steps))
	for i, s := range steps {
		if s.Name == "" {
			return ErrUnnamedStep
		}
		if _, ok := idx[s.Name]; ok {
			return fmt.Errorf("%w: %q", ErrDuplicateStep, s.Name)
		}
		idx[s.Name] = i
	}

	indegree := make([]int, len(steps))
	dependents := make([][]int, len(steps))
	for i, s := range steps {
		for _, dep := range s.DependsOn {
			j, ok := idx[dep]
			if !ok {
				return fmt.Errorf("%w: %q", ErrUnknownDependency, dep)
			}
			if j == i {
				return fmt.Errorf("%w: %q depends on itself", ErrDependencyCycle, s.Name)
			}
			indegree[i]++
			dependents[j] = append(dependents[j], i)
		}
	}

	// Kahn's algorithm: if a topological order does not cover every step, a cycle remains.
	queue := make([]int, 0, len(steps))
	for i, deg := range indegree {
		if deg == 0 {
			queue = append(queue, i)
		}
	}
	seen := 0
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		seen++
		for _, dep := range dependents[i] {
			indegree[dep]--
			if indegree[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}
	if seen != len(steps) {
		return ErrDependencyCycle
	}
	return nil
}
