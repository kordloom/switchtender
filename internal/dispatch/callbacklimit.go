package dispatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kordloom/switchtender/internal/inventory"
)

// LimitHosts returns the hosts of an inventory document that an Ansible limit pattern selects, as
// ansible-inventory evaluates it. It is how a provisioning callback that keeps its template's limit
// checks the calling host against that limit before anything launches.
//
// The pattern language is Ansible's: groups and their children, wildcards, regular expressions,
// subscripts, intersections, and exclusions, each with rules of its own. Ansible decides it rather
// than a second implementation of it, so the host a callback admits is exactly a host the same
// pattern reaches when the template launches.
//
// The document is written to a private directory for the one evaluation and removed straight after,
// the way a composed inventory's inputs are.
func (d *Dispatcher) LimitHosts(ctx context.Context, content, limit string) ([]string, error) {
	if d.invLister == nil {
		return nil, fmt.Errorf("%w: evaluating a host limit needs ansible-inventory on this server",
			inventory.ErrResolve)
	}
	dir, err := os.MkdirTemp("", "switchtender-limit-*")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", inventory.ErrResolve, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "inventory")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return nil, fmt.Errorf("%w: %w", inventory.ErrResolve, err)
	}
	out, err := d.invLister.ListInventory(ctx, []string{path}, limit)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", inventory.ErrResolve, err)
	}
	listing, err := inventory.ParseListing(out)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", inventory.ErrResolve, err)
	}
	return listing.Hosts(), nil
}
