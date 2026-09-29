// Package policy decides which runs must be held for approval, turning approval from an opt-in flag
// into an enforced rule. A policy matches runs by tool, command, and target; a matched run is held
// at submission so an operator cannot skip the gate.
package policy

import (
	"context"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/idgen"
	"github.com/kordloom/switchtender/internal/run"
)

// DisabledMaxDestroy is the MaxDestroy value that turns a policy's plan-content check off. It is the
// safe default, so a policy created without a destroy threshold never holds a run on plan content.
const DisabledMaxDestroy = -1

// Policy is a rule that requires approval for the runs it matches. Each criterion is optional; an
// empty criterion matches any value, so a policy with no criteria requires approval for every run.
// A policy is a blanket rule, held at submission, unless it sets a non-negative MaxDestroy, which
// makes it a plan-content rule enforced at execution by the plan gate instead.
type Policy struct {
	// ID is the unique policy identifier.
	ID string `json:"id"`
	// Name labels the policy for humans, for example "prod terraform destroy".
	Name string `json:"name"`
	// Tool matches a run's execution tool: ansible, bash, terraform, opentofu, python, powershell, or go. Empty matches any.
	Tool string `json:"tool,omitempty"`
	// CommandContains matches when a run's command contains this text. Empty matches any.
	CommandContains string `json:"command_contains,omitempty"`
	// InventoryID matches a run targeting this stored inventory. Empty matches any.
	InventoryID string `json:"inventory_id,omitempty"`
	// ExcludeDryRun leaves dry-run runs unmatched, so a no-change preview does not need approval.
	ExcludeDryRun bool `json:"exclude_dry_run,omitempty"`
	// MaxDestroy holds a matched terraform or opentofu run for approval when its plan would destroy
	// more than this many resources. A negative value disables the plan-content check, the safe
	// default, so a policy without a threshold is a blanket rule rather than one that holds on any
	// destroy.
	MaxDestroy int `json:"max_destroy"`
	// CreatedAt is when the policy was created.
	CreatedAt time.Time `json:"created_at"`
}

// Matches reports whether the policy requires approval for r. Every non-empty criterion must match,
// so a policy narrows the runs it gates rather than widening them.
func (p *Policy) Matches(r *run.Run) bool {
	if p.ExcludeDryRun && r.DryRun {
		return false
	}
	if p.Tool != "" && run.NormalizeTool(p.Tool) != run.NormalizeTool(r.Tool) {
		return false
	}
	if p.CommandContains != "" && !strings.Contains(r.Command, p.CommandContains) {
		return false
	}
	if p.InventoryID != "" && p.InventoryID != r.InventoryID {
		return false
	}
	return true
}

// Requires reports whether any blanket policy requires approval for r at submission. A plan-content
// policy, one with a non-negative MaxDestroy, is not a blanket rule: it is enforced at execution by
// the plan gate, which plans the run and holds the apply only when its plan destroys too much, so it
// is skipped here.
func Requires(policies []*Policy, r *run.Run) bool {
	for _, p := range policies {
		if p.MaxDestroy < 0 && p.Matches(r) {
			return true
		}
	}
	return false
}

// PlanGated reports whether any plan-content policy scopes r, meaning r's apply must be planned and
// checked before it runs. A policy is plan-content when its MaxDestroy is non-negative, and it scopes
// r when it also matches r. This is separate from Matches, which is unchanged.
func PlanGated(policies []*Policy, r *run.Run) bool {
	for _, p := range policies {
		if p.MaxDestroy >= 0 && p.Matches(r) {
			return true
		}
	}
	return false
}

// PlanExceeds reports whether a plan that destroys the given number of resources violates any
// plan-content policy scoping r. A policy participates only when it is enabled, its MaxDestroy
// non-negative, and it matches r; it is violated when destroys is greater than its threshold. A run
// whose plan exceeds a threshold is held for approval rather than applied.
func PlanExceeds(policies []*Policy, r *run.Run, destroys int) bool {
	for _, p := range policies {
		if p.MaxDestroy >= 0 && p.Matches(r) && destroys > p.MaxDestroy {
			return true
		}
	}
	return false
}

// Store persists approval policies. Implementations must be safe for concurrent use.
type Store interface {
	// Save stores a policy, inserting or replacing by id.
	Save(ctx context.Context, p *Policy) error
	// Get returns the policy with the given id, or ErrNotFound when it does not exist.
	Get(ctx context.Context, id string) (*Policy, error)
	// List returns every policy, oldest first.
	List(ctx context.Context) ([]*Policy, error)
	// Delete removes a policy by id, returning ErrNotFound when it does not exist.
	Delete(ctx context.Context, id string) error
}

// NewPolicy returns a policy with the given name and a fresh id, its plan-content check disabled so a
// policy created without a destroy threshold never holds a run on plan content. Callers set the
// matching criteria and, to enable the plan-content gate, a non-negative MaxDestroy.
func NewPolicy(name string) *Policy {
	return &Policy{ID: NewID(), Name: name, MaxDestroy: DisabledMaxDestroy, CreatedAt: time.Now()}
}

// NewID returns a random policy identifier prefixed with "pol_".
func NewID() string {
	return idgen.New("pol_", 6)
}
