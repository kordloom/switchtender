package dispatch

import (
	"context"
	"fmt"

	"github.com/dcadolph/yardmaster/internal/run"
)

// stepState tracks where a pipeline step is in the graph walk.
type stepState int

const (
	// stepWaiting means the step has not started and its dependencies are not settled.
	stepWaiting stepState = iota
	// stepRunning means the step's child run is executing.
	stepRunning
	// stepDone means the step's child run reached a terminal status.
	stepDone
	// stepSkipped means the step never ran because a dependency failed, was skipped, or the
	// pipeline was canceled first.
	stepSkipped
)

// stepResult carries one step's terminal status back to the graph walk.
type stepResult struct {
	// idx is the step's declaration index.
	idx int
	// status is the child run's terminal status.
	status run.Status
}

// hasDependencies reports whether any step declares a dependency, which switches the pipeline
// from ordered execution to a graph walk.
func hasDependencies(steps []run.PipelineStep) bool {
	for _, s := range steps {
		if len(s.DependsOn) > 0 {
			return true
		}
	}
	return false
}

// validateDAG checks that a dependency declaring pipeline has uniquely named steps, that every
// dependency references a known step, and that the graph has no cycles.
func validateDAG(steps []run.PipelineStep) error {
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

// runStepsDAG executes the steps as a dependency graph. Steps whose dependencies are settled run
// concurrently through the worker pool. A step runs when every dependency succeeded, or failed
// with continue on failure set; otherwise the step is skipped and creates no run. It returns
// whether any step failed and whether execution was canceled.
func (d *Dispatcher) runStepsDAG(ctx context.Context, parent *run.Run, steps []run.PipelineStep) (failed, canceled bool) {
	byName := make(map[string]int, len(steps))
	for i, s := range steps {
		byName[s.Name] = i
	}
	states := make([]stepState, len(steps))
	results := make([]run.Status, len(steps))
	done := make(chan stepResult, len(steps))
	running := 0

	// depSettled reports whether the dependency at j is finished for good, and depAllows whether
	// its outcome lets a dependent run.
	depSettled := func(j int) bool { return states[j] == stepDone || states[j] == stepSkipped }
	depAllows := func(j int) bool {
		if states[j] != stepDone {
			return false
		}
		return results[j] == run.StatusSucceeded || steps[j].ContinueOnFailure
	}

	launch := func(i int) {
		states[i] = stepRunning
		running++
		idx := i
		step := steps[i]
		go func() {
			done <- stepResult{idx: idx, status: d.runStepAttempts(ctx, parent, step, idx)}
		}()
	}

	for {
		// Settle everything that can start or can never start, repeating until quiescent since
		// one skip can unblock the decision for its dependents.
		progress := true
		for progress {
			progress = false
			for i := range steps {
				if states[i] != stepWaiting {
					continue
				}
				if ctx.Err() != nil {
					states[i] = stepSkipped
					canceled = true
					progress = true
					continue
				}
				ready := true
				blocked := false
				for _, dep := range steps[i].DependsOn {
					j := byName[dep]
					if !depSettled(j) {
						ready = false
						continue
					}
					if states[j] == stepSkipped || !depAllows(j) {
						blocked = true
					}
				}
				switch {
				case blocked:
					states[i] = stepSkipped
					progress = true
				case ready:
					launch(i)
					progress = true
				}
			}
		}

		if running == 0 {
			break
		}
		res := <-done
		running--
		states[res.idx] = stepDone
		results[res.idx] = res.status
		switch res.status {
		case run.StatusCanceled:
			canceled = true
		case run.StatusSucceeded:
		default:
			failed = true
		}
	}
	return failed, canceled
}
