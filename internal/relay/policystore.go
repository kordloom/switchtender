package relay

import (
	"context"
	"fmt"

	"github.com/kordloom/switchtender/internal/policy"
)

// PolicyClient is a policy.Store a relay worker runs against, reading the approval policies in force
// from the control node instead of from a database it cannot reach.
//
// The plan-content gate runs where the run executes, so a worker with no view of the policies cannot
// enforce it. Handing a worker no store at all made the gate silently vanish: a terraform apply
// scoped by a destroy threshold was planned and held when the control node claimed it, and applied
// straight to production when a worker won the race. Refusing every read instead failed closed, but
// it failed closed on every terraform run in the install, including the ones no policy would ever
// have matched. Reading the real policies is what makes the gate mean the same thing in both places.
type PolicyClient struct {
	// t carries the read to the control node.
	t Transport
}

// NewPolicyClient returns a policy.Store backed by the Transport, ready to hand to dispatch.
func NewPolicyClient(t Transport) *PolicyClient {
	if t == nil {
		panic("relay: Transport required")
	}
	return &PolicyClient{t: t}
}

// compile-time proof that a PolicyClient is a policy.Store.
var _ policy.Store = (*PolicyClient)(nil)

// List returns the approval policies in force on the control node.
//
// An error here is not read as "no policies". The caller fails the run closed, because a gate that
// could not be evaluated has not been passed.
func (c *PolicyClient) List(ctx context.Context) ([]*policy.Policy, error) {
	all, err := c.t.Policies(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", policy.ErrUnreachable, err)
	}
	return all, nil
}

// Get returns the policy with the given id, or ErrNotFound.
func (c *PolicyClient) Get(ctx context.Context, id string) (*policy.Policy, error) {
	all, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range all {
		if p.ID == id {
			return p, nil
		}
	}
	return nil, policy.ErrNotFound
}

// Save refuses. A worker reads the policies the control node holds and never authors them.
func (c *PolicyClient) Save(context.Context, *policy.Policy) error {
	return fmt.Errorf("%w: a worker reads approval policies and does not write them",
		policy.ErrReadOnly)
}

// Delete refuses, for the same reason as Save.
func (c *PolicyClient) Delete(context.Context, string) error {
	return fmt.Errorf("%w: a worker reads approval policies and does not remove them",
		policy.ErrReadOnly)
}
