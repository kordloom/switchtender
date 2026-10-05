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
