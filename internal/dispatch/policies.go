package dispatch

import (
	"context"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// WithPolicies enforces approval policies on submitted runs: a run matching any stored policy is
// held for approval instead of dispatched, so the gate cannot be skipped by omitting the flag.
func WithPolicies(store policy.Store) Option {
	return func(c *config) { c.policies = store }
}

// requiresApproval reports whether any stored policy requires approval for r. A lookup failure is
// logged and treated as no match, since a policy store failure implies a broader store failure that
// the run's own save would surface.
func (d *Dispatcher) requiresApproval(ctx context.Context, r *run.Run) bool {
	if d.policies == nil {
		return false
	}
	policies, err := d.policies.List(ctx)
	if err != nil {
		d.log.Error("dispatch: list policies: " + err.Error())
		return false
	}
	return policy.Requires(policies, r)
}

// pipelineRequiresApproval reports whether a pipeline must be held, which it must when the parent
// itself matches a blanket policy or when any of its steps does. A pipeline is submitted through a
// different path than a single run, so without this the same command an operator gated would execute
// freely by being wrapped in a one-step workflow. The whole pipeline is held rather than the matching
// step, because the graph walk cannot park a step midway and a partly applied change is worse than
// one that never started.
func (d *Dispatcher) pipelineRequiresApproval(ctx context.Context, parent *run.Run,
	steps []run.PipelineStep) bool {
	if d.policies == nil {
		return false
	}
	policies, err := d.policies.List(ctx)
	if err != nil {
		d.log.Error("dispatch: list policies: " + err.Error())
		return false
	}
	if policy.Requires(policies, parent) {
		return true
	}
	for i, step := range steps {
		if policy.Requires(policies, stepRun(parent, step, i, 0, nil)) {
			return true
		}
	}
	return false
}
