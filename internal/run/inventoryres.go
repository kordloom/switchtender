package run

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

// InventoryResolution is what a composed inventory stood for at one launch: which kind it was, the
// input inventories that contributed hosts, and every host it resolved to.
type InventoryResolution struct {
	// Kind is the composed inventory's kind, smart or constructed.
	Kind string `json:"kind"`
	// Inputs are the ids of the input inventories that contributed at least one host, in the order
	// they were read. An input that matched nothing is left out, so a rule scoped to an inventory
	// is not drawn into a run that never touched its hosts.
	Inputs []string `json:"inputs,omitempty"`
	// Hosts is every host the inventory resolved to, sorted.
	Hosts []string `json:"hosts"`
	// Engine is what resolved it: native when the engine in this binary read every input, ansible
	// when Ansible read any of it, as it always does for a constructed inventory.
	Engine string `json:"engine,omitempty"`
	// AnsibleCore is the ansible-core version that resolved it, when Ansible did.
	AnsibleCore string `json:"ansible_core,omitempty"`
	// InputDigest is the digest of the inputs it drew hosts from, each by id and its content as the
	// API serves it, secrets masked.
	InputDigest string `json:"input_digest,omitempty"`
	// ResolvedDigest is the digest of the inventory it resolved to: hosts, groups, and variables,
	// secret values masked.
	ResolvedDigest string `json:"resolved_digest,omitempty"`
}

// Clone returns a deep copy, or nil for nil.
func (r *InventoryResolution) Clone() *InventoryResolution {
	if r == nil {
		return nil
	}
	out := *r
	out.Inputs = slices.Clone(r.Inputs)
	out.Hosts = slices.Clone(r.Hosts)
	return &out
}

// InventoryCheck is the record an Ansible run against a natively resolved inventory leaves of the
// cross-check made just before it executed: Ansible's own reading of every input and of the
// inventory handed to the play, compared with the native engine's. A run whose readings disagree is
// refused, and the differences are what it records.
type InventoryCheck struct {
	// AnsibleCore is the ansible-core version that read the inventory on the executor.
	AnsibleCore string `json:"ansible_core"`
	// InputDigest is the digest of the inputs as execution read them, comparable to the
	// resolution's InputDigest: they match unless an input changed between launch and execution.
	InputDigest string `json:"input_digest,omitempty"`
	// ResolvedDigest is the digest of the inventory the run executed against, as both engines read
	// it.
	ResolvedDigest string `json:"resolved_digest,omitempty"`
	// Differences lists where the two readings disagreed, secrets withheld. Empty when they agreed.
	Differences []string `json:"differences,omitempty"`
}

// Clone returns a deep copy, or nil for nil.
func (c *InventoryCheck) Clone() *InventoryCheck {
	if c == nil {
		return nil
	}
	out := *c
	out.Differences = slices.Clone(c.Differences)
	return &out
}

// CheckColumn encodes an inventory check for a store column, empty for none.
func CheckColumn(c *InventoryCheck) string {
	if c == nil {
		return ""
	}
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return string(b)
}

// ParseCheckColumn decodes a stored inventory check. An empty column is a run that made none. A
// column that does not decode is reported rather than read as empty, because the check is evidence.
func ParseCheckColumn(s string) (*InventoryCheck, error) {
	if s == "" {
		return nil, nil
	}
	var out InventoryCheck
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("parse inventory check: %w", err)
	}
	return &out, nil
}

// TargetsInventory reports whether a run reaches the hosts of the stored inventory id, either by
// naming it directly or through a composed inventory that drew hosts from it.
//
// A rule scoped to an inventory exists to govern the machines in it. Matching only the id a run
// names let a smart inventory over the same machines walk around every such rule, so the hosts a
// composed inventory drew from an input count as targeting that input.
func (r *Run) TargetsInventory(id string) bool {
	if id == "" || r == nil {
		return false
	}
	if r.InventoryID == id {
		return true
	}
	return r.InventoryResolution != nil && slices.Contains(r.InventoryResolution.Inputs, id)
}

// InventoryAccessFunc reports whether the actor carried on ctx may use the stored inventory id.
type InventoryAccessFunc func(ctx context.Context, inventoryID string) (bool, error)

// accessKey types the context value carrying the inventory access check of the request in flight.
type accessKey struct{}

// WithInventoryAccess carries the check that decides which input inventories a composed inventory
// may draw hosts from for the actor making the request. It is set by the auth gate beside the
// actor, so a run launched by a person only ever reaches hosts that person could have targeted
// directly. A nil check is a no-op.
func WithInventoryAccess(ctx context.Context, check InventoryAccessFunc) context.Context {
	if check == nil {
		return ctx
	}
	return context.WithValue(ctx, accessKey{}, check)
}

// InventoryAccessFrom returns the inventory access check carried on ctx, nil when the work was not
// started by an actor, such as a schedule firing.
func InventoryAccessFrom(ctx context.Context) InventoryAccessFunc {
	check, _ := ctx.Value(accessKey{}).(InventoryAccessFunc)
	return check
}

// ResolutionColumn encodes a resolution for a store column, empty for none, so an ordinary run
// stores nothing rather than a JSON null.
func ResolutionColumn(r *InventoryResolution) string {
	if r == nil {
		return ""
	}
	b, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	return string(b)
}

// ParseResolutionColumn decodes a stored resolution. An empty column is a run that targeted no
// composed inventory. A column that does not decode is reported rather than read as empty, because
// the resolution is evidence: reading a corrupted one as absent would present a run whose target
// set was edited as a run that never had one.
func ParseResolutionColumn(s string) (*InventoryResolution, error) {
	if s == "" {
		return nil, nil
	}
	var out InventoryResolution
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("parse inventory resolution: %w", err)
	}
	return &out, nil
}
