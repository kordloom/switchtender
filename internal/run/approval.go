package run

import (
	"context"
	"sort"
)

// ApprovalSteps returns the approval step records of a pipeline parent, ordered by step index. A
// parent that never started has none, which is how a workflow held before any step ran is told
// apart from one paused partway through.
func ApprovalSteps(ctx context.Context, store Store, parentID string) ([]*Run, error) {
	children, err := store.Steps(ctx, parentID)
	if err != nil {
		return nil, err
	}
	out := make([]*Run, 0, len(children))
	for _, c := range children {
		if c.Kind == KindApproval {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return stepIndex(out[i]) < stepIndex(out[j]) })
	return out, nil
}

// PendingApprovalSteps returns the approval steps of a pipeline parent still waiting for a
// decision.
func PendingApprovalSteps(ctx context.Context, store Store, parentID string) ([]*Run, error) {
	steps, err := ApprovalSteps(ctx, store, parentID)
	if err != nil {
		return nil, err
	}
	out := steps[:0]
	for _, s := range steps {
		if s.Status == StatusPendingApproval {
			out = append(out, s)
		}
	}
	return out, nil
}

// Started reports whether a pipeline parent has begun walking its graph, which it has once any step
// record exists. A parent held for approval before it started has none, and approving that hold
// runs the graph from the top. A parent that started and is now held is paused at an approval step,
// and running it from the top would repeat every step that already changed something.
func Started(ctx context.Context, store Store, parentID string) (bool, error) {
	children, err := store.Steps(ctx, parentID)
	if err != nil {
		return false, err
	}
	return len(children) > 0, nil
}

// stallState is where one step of a workflow stands as StalledAtApproval reads its records.
type stallState int

const (
	// stallWaiting is a step with no record whose dependencies have not all settled.
	stallWaiting stallState = iota
	// stallDone is a step whose record is final, a decision the workflow acted on included.
	stallDone
	// stallSkipped is a step with no record that the outcome of a dependency rules out.
	stallSkipped
	// stallAsking is an approval step waiting for a person, or decided and not acted on yet.
	stallAsking
)

// StalledAtApproval reports whether a workflow whose coordinator stopped renewing its lease had
// nothing left to do but wait for a person, read from parent and its step records: no step
// executing or ready to start, and an approval step the workflow asked and has not acted on, still
// waiting for a decision or decided since. It is the rule the lease sweep parks a workflow by
// instead of interrupting it, stated once here so every store applies the same one.
//
// The moment it covers is the one between a workflow opening an approval step, which lists the step
// for a decision, and its park, which releases its lease. A process that died there left the
// workflow running under a lease nobody renewed while its step could still be decided, and the sweep
// ended it as interrupted, losing a decision that had already been accepted.
//
// A decided step counts as not acted on while no step after it has a record, since the walk records
// a step the moment a decision lets it start. A workflow with a cancel requested, or with any other
// step executing or ready to start, was not waiting for a person, and is interrupted as before.
func StalledAtApproval(parent *Run, steps []*Run) bool {
	if parent.Kind != KindPipeline || parent.CancelRequested {
		return false
	}
	graph := GraphSteps(parent.Steps)
	byName := make(map[string]int, len(graph))
	for i, s := range graph {
		byName[s.Name] = i
	}
	latest := make([]*Run, len(graph))
	for _, c := range steps {
		if !c.Status.Terminal() && (c.Kind != KindApproval || c.Status != StatusPendingApproval) {
			return false
		}
		if c.StepIndex == nil || *c.StepIndex < 0 || *c.StepIndex >= len(graph) {
			continue
		}
		prev := latest[*c.StepIndex]
		if prev == nil || c.Attempt > prev.Attempt ||
			(c.Attempt == prev.Attempt && c.CreatedAt.After(prev.CreatedAt)) {
			latest[*c.StepIndex] = c
		}
	}
	deps := stepClosures(graph, byName)
	state := make([]stallState, len(graph))
	asked := false
	for i, c := range latest {
		switch {
		case c == nil:
		case c.Kind == KindApproval && (!c.Status.Terminal() || !actedOn(i, latest, deps)):
			state[i], asked = stallAsking, true
		default:
			state[i] = stallDone
		}
	}
	if !asked {
		return false
	}
	// The steps with no record are settled the way the walk settles them: one a dependency rules out
	// is skipped, an approval step that could be asked waits for a person as well, and one that could
	// start means the workflow was not waiting for anybody.
	for progress := true; progress; {
		progress = false
		for i := range graph {
			if state[i] != stallWaiting {
				continue
			}
			ready, blocked := stallReadiness(graph, byName, state, latest, i)
			switch {
			case blocked:
				state[i] = stallSkipped
				progress = true
			case ready && graph[i].IsApproval():
				state[i] = stallAsking
				progress = true
			case ready:
				return false
			}
		}
	}
	return true
}

// actedOn reports whether the workflow acted on the decided approval step at i, which it has once
// any step after it has a record.
func actedOn(i int, latest []*Run, deps [][]bool) bool {
	for j, c := range latest {
		if c != nil && deps[j][i] {
			return true
		}
	}
	return false
}

// stallReadiness reports whether the step at i, which has no record, could start from what its
// dependencies' records say, and whether it never can. It decides as the dispatcher's walk does: a
// step waits for every step it depends on to succeed, or to fail with continue on failure set, and
// for every approval step whose denial it handles to be denied.
func stallReadiness(graph []PipelineStep, byName map[string]int, state []stallState, latest []*Run,
	i int) (ready, blocked bool) {
	ready = true
	for _, dep := range graph[i].DependsOn {
		j := byName[dep]
		switch state[j] {
		case stallSkipped:
			blocked = true
		case stallDone:
			s := latest[j].Status
			if s != StatusSucceeded && (graph[j].IsApproval() || !graph[j].ContinueOnFailure) {
				blocked = true
			}
		default:
			ready = false
		}
	}
	for _, dep := range graph[i].IfDenied {
		j := byName[dep]
		switch state[j] {
		case stallSkipped:
			blocked = true
		case stallDone:
			if s := latest[j].Status; s != StatusRejected && s != StatusFailed {
				blocked = true
			}
		default:
			ready = false
		}
	}
	return ready, blocked
}

// stepClosures returns, per step, which steps it transitively depends on, over both the steps it
// waits for and the approval steps whose denial it handles.
func stepClosures(graph []PipelineStep, byName map[string]int) [][]bool {
	closures := make([][]bool, len(graph))
	var build func(i int) []bool
	build = func(i int) []bool {
		if closures[i] != nil {
			return closures[i]
		}
		closure := make([]bool, len(graph))
		closures[i] = closure
		for _, dep := range append(append([]string(nil), graph[i].DependsOn...), graph[i].IfDenied...) {
			j, ok := byName[dep]
			if !ok {
				continue
			}
			closure[j] = true
			for k, in := range build(j) {
				if in {
					closure[k] = true
				}
			}
		}
		return closure
	}
	for i := range graph {
		build(i)
	}
	return closures
}
