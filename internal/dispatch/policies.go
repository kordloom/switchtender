package dispatch

import (
	"context"

	"github.com/dcadolph/switchtender/internal/policy"
	"github.com/dcadolph/switchtender/internal/run"
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
