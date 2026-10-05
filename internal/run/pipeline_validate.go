package run

import (
	"errors"
	"fmt"
	"slices"
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
	// ErrApprovalStep is returned when an approval step carries something only a tool step can use,
	// or a tool step carries something only an approval step can use.
	ErrApprovalStep = errors.New("approval step invalid")
	// ErrDenyPath is returned when a step's if_denied names something other than an approval step.
	ErrDenyPath = errors.New("deny path invalid")
)

// MaxApprovalDescription bounds the text an approval step shows its approver, so a step cannot
// carry an unbounded document into every listing and notification that names it.
const MaxApprovalDescription = 2000

// ValidatePipeline checks that a pipeline is legal: it has between one and MaxPipelineSteps steps,
// each step carries the input its tool needs, and, when any step declares a dependency, the steps
// are uniquely named and their dependencies form a directed acyclic graph.
//
// The name and graph checks apply only when a dependency, a deny path, or an approval step is
// declared, so a plain sequence of steps need not name them, which is how a linear pipeline has
// always been accepted. An approval step has to be named because an approver is shown the name and
// decides it by the name. The per-step input check always applies. This is the single definition
// both the dispatcher and a saved workflow template validate through.
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
	if pipelineHasDependencies(steps) || HasApproval(steps) {
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
	switch s.Type {
	case StepApproval:
		return validateApprovalStep(s, label)
	case "":
	default:
		return fmt.Errorf("%w: %s names an unknown step type %q", ErrApprovalStep, label, s.Type)
	}
	if s.ApprovalTimeout != 0 || s.Description != "" {
		return fmt.Errorf("%w: %s runs a tool, so a description and an approval timeout do not "+
			"apply to it", ErrApprovalStep, label)
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

// validateApprovalStep checks an approval step. It runs nothing, so every field that shapes an
// execution is refused rather than ignored: a playbook or a retry count on an approval step reads
// as work that will happen, and none will. Continue on failure is refused for a sharper reason,
// since it would let the steps after an approval run when the approval was denied, which is no gate
// at all.
func validateApprovalStep(s PipelineStep, label string) error {
	if s.Name == "" {
		return fmt.Errorf("%w: an approval step needs a name, which is what its approver is shown",
			ErrApprovalStep)
	}
	if s.Tool != "" || s.Playbook != "" || s.Command != "" || s.Inventory != "" || s.DryRun ||
		s.Retries != 0 {
		return fmt.Errorf("%w: %s waits for a person and runs nothing, so it takes no tool, "+
			"playbook, command, inventory, dry run, or retries", ErrApprovalStep, label)
	}
	if s.ContinueOnFailure {
		return fmt.Errorf("%w: %s cannot continue on failure, because that would run the steps "+
			"after it when the approval is denied. Give it a deny path with if_denied instead",
			ErrApprovalStep, label)
	}
	if s.ApprovalTimeout < 0 {
		return fmt.Errorf("%w: %s has a negative approval timeout", ErrApprovalStep, label)
	}
	if len(s.Description) > MaxApprovalDescription {
		return fmt.Errorf("%w: %s has a description longer than %d characters", ErrApprovalStep,
			label, MaxApprovalDescription)
	}
	return nil
}

// pipelineHasDependencies reports whether any step declares a dependency or a deny path, which
// turns the pipeline from a sequence into a graph.
func pipelineHasDependencies(steps []PipelineStep) bool {
	for _, s := range steps {
		if len(s.DependsOn) > 0 || len(s.IfDenied) > 0 {
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
		for _, dep := range s.IfDenied {
			j, ok := idx[dep]
			if !ok {
				return fmt.Errorf("%w: %q", ErrUnknownDependency, dep)
			}
			if j == i {
				return fmt.Errorf("%w: %q handles its own denial", ErrDependencyCycle, s.Name)
			}
			if !steps[j].IsApproval() {
				return fmt.Errorf("%w: %q names %q in if_denied, which is not an approval step",
					ErrDenyPath, s.Name, dep)
			}
			// Waiting for an approval and handling its denial are opposite outcomes of one decision,
			// so a step naming both could never run.
			if slices.Contains(s.DependsOn, dep) {
				return fmt.Errorf("%w: %q waits for %q to be approved and to be denied, so it "+
					"could never run", ErrDenyPath, s.Name, dep)
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
