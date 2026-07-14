// Package policy decides which runs must be held for approval, turning approval from an opt-in flag
// into an enforced rule. A policy matches runs by tool, command, and target; a matched run is held
// at submission so an operator cannot skip the gate.
package policy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/dcadolph/railwarden/internal/run"
)

// Policy is a rule that requires approval for the runs it matches. Each criterion is optional; an
// empty criterion matches any value, so a policy with no criteria requires approval for every run.
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

// Requires reports whether any policy requires approval for r.
func Requires(policies []*Policy, r *run.Run) bool {
	for _, p := range policies {
		if p.Matches(r) {
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

// NewID returns a random policy identifier prefixed with "pol_".
func NewID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("policy: read random: " + err.Error())
	}
	return "pol_" + hex.EncodeToString(b[:])
}
