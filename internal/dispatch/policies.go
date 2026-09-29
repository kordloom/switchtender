package dispatch

import (
	"context"
	"fmt"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// WithPolicies enforces approval policies on submitted runs: a run matching any stored policy is
// held for approval instead of dispatched, so the gate cannot be skipped by omitting the flag.
func WithPolicies(store policy.Store) Option {
	return func(c *config) { c.policies = store }
}

// requiresApproval reports whether any stored policy requires approval for r, and refuses the
// submission when it cannot tell.
//
// A lookup failure used to be logged and treated as no match, on the reasoning that a policy store
// failure implied a broader store failure the run's own save would surface. That held while policies
// were rows in the same database as runs. It stopped holding the moment policies could come from a
// file: a deleted file, a half-written deploy, or one typo in a merged change makes every gate
// disappear while runs save perfectly well, and the only signal is one log line per submission.
//
// A gate that cannot be evaluated is not a gate that passed. The submission fails instead, loudly,
// which is disruptive exactly once and in the direction that does not execute something an approver
// was meant to see.
func (d *Dispatcher) requiresApproval(ctx context.Context, r *run.Run) (bool, error) {
	if d.policies == nil {
		return false, nil
	}
	policies, err := d.policies.List(ctx)
	if err != nil {
		d.log.Error("dispatch: list policies: " + err.Error())
		return false, fmt.Errorf("%w: approval policies could not be read, so the run is refused "+
			"rather than run past a gate that could not be checked: %w", ErrPolicyUnavailable, err)
	}
	return policy.Requires(policies, r), nil
}

// pipelineRequiresApproval reports whether a pipeline must be held, which it must when the parent
// itself matches a blanket policy or when any of its steps does. A pipeline is submitted through a
// different path than a single run, so without this the same command an operator gated would execute
// freely by being wrapped in a one-step workflow. The whole pipeline is held rather than the matching
// step, because the graph walk cannot park a step midway and a partly applied change is worse than
// one that never started.
func (d *Dispatcher) pipelineRequiresApproval(ctx context.Context, parent *run.Run,
	steps []run.PipelineStep) (bool, error) {
	if d.policies == nil {
		return false, nil
	}
	policies, err := d.policies.List(ctx)
	if err != nil {
		d.log.Error("dispatch: list policies: " + err.Error())
		return false, fmt.Errorf("%w: approval policies could not be read, so the pipeline is "+
			"refused rather than run past a gate that could not be checked: %w",
			ErrPolicyUnavailable, err)
	}
	if policy.Requires(policies, parent) {
		return true, nil
	}
	for i, step := range steps {
		if policy.Requires(policies, stepRun(parent, step, i, 0, nil)) {
			return true, nil
		}
	}
	return false, nil
}
