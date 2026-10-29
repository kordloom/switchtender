package policytest

import (
	"context"
	"fmt"

	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/policy"
)

// serving is a read-only policy store serving a fixed rule set, Rego policies included, the way a
// policy file serves them.
type serving struct {
	// rules are the policies the store serves, in list order.
	rules []*policy.Policy
}

// Serving returns a read-only store serving rules. Unlike a memory store it accepts a Rego policy,
// so a test can put one in front of a dispatcher or a server without the paid compiler.
func Serving(rules ...*policy.Policy) policy.Store {
	return serving{rules: rules}
}

// Refusing returns a Rego policy named name whose program was never compiled. Every build refuses
// every run on it, because a gate that cannot be evaluated has not been passed, so a test that needs
// a refusal gets one without a paid rule.
func Refusing(name string) *policy.Policy {
	return &policy.Policy{ID: "pol_" + name, Name: name, MaxDestroy: policy.DisabledMaxDestroy,
		Rego: &policy.RegoProgram{}}
}

// CommunityHold returns the name a Community build records on the hold it places for an advanced
// rule it cannot evaluate: the rule's name and the tier line for f.
func CommunityHold(name string, f license.Feature) string {
	return fmt.Sprintf("%s (held because this build cannot evaluate it: %s)", name,
		license.Unlicensed(f).Error())
}

// Save refuses, since the store is read-only.
func (serving) Save(context.Context, *policy.Policy) error { return policy.ErrReadOnly }

// Get returns the served policy with the given id.
func (s serving) Get(_ context.Context, id string) (*policy.Policy, error) {
	for _, p := range s.rules {
		if p.ID == id {
			return p, nil
		}
	}
	return nil, policy.ErrNotFound
}

// List returns the served policies.
func (s serving) List(context.Context) ([]*policy.Policy, error) { return s.rules, nil }

// Delete refuses, since the store is read-only.
func (serving) Delete(context.Context, string) error { return policy.ErrReadOnly }
