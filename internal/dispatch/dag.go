package dispatch

import (
	"context"
	"maps"

	"github.com/kordloom/switchtender/internal/run"
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

// stepResult carries one step's terminal status and published outputs back to the graph walk.
type stepResult struct {
	// idx is the step's declaration index.
	idx int
	// status is the child run's terminal status.
	status run.Status
	// outputs holds the values the step published for its dependents.
	outputs map[string]any
}

// depClosures returns, per step, which steps it transitively depends on.
func depClosures(steps []run.PipelineStep, byName map[string]int) [][]bool {
	closures := make([][]bool, len(steps))
	var build func(i int) []bool
	build = func(i int) []bool {
		if closures[i] != nil {
			return closures[i]
		}
		closure := make([]bool, len(steps))
		closures[i] = closure
		for _, dep := range steps[i].DependsOn {
			j := byName[dep]
			closure[j] = true
			for k, in := range build(j) {
				if in {
					closure[k] = true
				}
			}
		}
		return closure
	}
	for i := range steps {
		build(i)
	}
	return closures
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

// runStepsDAG executes the steps as a dependency graph. Steps whose dependencies are settled run
// concurrently through the worker pool. A step runs when every dependency succeeded, or failed
// with continue on failure set; otherwise the step is skipped and creates no run. Each step
// receives the merged outputs of its transitive dependencies as extra vars. It returns whether
// any step failed and whether execution was canceled.
func (d *Dispatcher) runStepsDAG(ctx context.Context, parent *run.Run, steps []run.PipelineStep) (failed, canceled bool) {
	byName := make(map[string]int, len(steps))
	for i, s := range steps {
		byName[s.Name] = i
	}
	closures := depClosures(steps, byName)
	states := make([]stepState, len(steps))
	results := make([]run.Status, len(steps))
	outputs := make([]map[string]any, len(steps))
	done := make(chan stepResult, len(steps))
	running := 0

	// inputsFor merges the outputs of the step's transitive dependencies in declaration order, so
	// the result does not depend on which branch finished first.
	inputsFor := func(i int) map[string]any {
		// The parent's own vars are the base every step starts from, so a workflow's survey answers
		// reach each step; a transitive dependency's outputs are layered on top in declaration order,
		// so a published output overrides a parent var of the same name and the result does not
		// depend on which branch finished first.
		vars := baseStepVars(parent)
		for j := range steps {
			if !closures[i][j] || len(outputs[j]) == 0 {
				continue
			}
			maps.Copy(vars, outputs[j])
		}
		return vars
	}

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
		vars := inputsFor(i)
		go func() {
			status, published := d.runStepAttempts(ctx, parent, step, idx, vars)
			done <- stepResult{idx: idx, status: status, outputs: published}
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
		outputs[res.idx] = res.outputs
		switch res.status {
		// A step the server stopped ends the graph the same way a canceled one does, rather than
		// counting as a failure the run never actually reached.
		case run.StatusCanceled, run.StatusInterrupted:
			canceled = true
		case run.StatusSucceeded:
		default:
			failed = true
		}
	}
	return failed, canceled
}
