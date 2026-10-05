package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// renderedName is the name the inventory handed to the play is checked under.
const renderedName = "inventory"

// crossCheckInventory holds an Ansible run against a natively resolved inventory to Ansible's own
// reading of it. Every input the native engine read is read again by ansible-inventory, and so is
// the inventory rendered for the play, all with the run's environment, its project's ansible.cfg,
// and its image, and each reading must match the native one in hosts, groups, and variables. A
// disagreement refuses the run, naming every difference, so a play never executes against a host
// set or a variable the evidence does not describe. The check is fail-closed: an executor that
// cannot read the inventory with Ansible refuses the run too.
//
// It runs only for an Ansible run whose composed inventory the native engine read at least in part.
// The result is stamped on the run, which the outcome record commits. mask redacts each difference
// with the run's known secrets.
func (d *Dispatcher) crossCheckInventory(ctx context.Context, r *run.Run, spec roundhouse.Spec,
	comp *composed, mask func(string) string) error {
	if comp == nil || run.NormalizeTool(r.Tool) != run.ToolAnsible {
		return nil
	}
	var natives []*composeInput
	for _, in := range comp.inputs {
		if in.engine == inventory.EngineNative && in.listing != nil {
			natives = append(natives, in)
		}
	}
	if len(natives) == 0 {
		return nil
	}
	if d.invReader == nil {
		return fmt.Errorf("%w: %w: this executor cannot read the inventory with Ansible to check "+
			"the native engine's resolution, so the run is refused rather than run unchecked",
			inventory.ErrResolve, inventory.ErrNeedsAnsible)
	}
	dir, err := os.MkdirTemp("", "switchtender-invcheck-*")
	if err != nil {
		return fmt.Errorf("%w: %w", inventory.ErrResolve, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	names := make([]string, 0, len(natives)+1)
	for n, in := range natives {
		name := fmt.Sprintf("input-%03d", n)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(in.content), 0o600); err != nil {
			return fmt.Errorf("%w: %w", inventory.ErrResolve, err)
		}
		names = append(names, name)
	}
	if err := os.WriteFile(filepath.Join(dir, renderedName), []byte(comp.Content), 0o600); err != nil {
		return fmt.Errorf("%w: %w", inventory.ErrResolve, err)
	}
	names = append(names, renderedName)
	outs, version, err := d.invReader.ReadInventories(ctx, spec, dir, names)
	if errors.Is(err, roundhouse.ErrAnsibleMissing) {
		return fmt.Errorf("%w: %w: an Ansible run against an inventory the native engine resolved is "+
			"checked against Ansible's own reading first, and ansible-inventory is not installed on "+
			"this executor. Install it with: %s", inventory.ErrResolve, inventory.ErrNeedsAnsible,
			inventory.AnsibleInstallHint)
	}
	if err != nil {
		return fmt.Errorf("%w: Ansible could not read the inventory to check it: %s",
			inventory.ErrResolve, mask(err.Error()))
	}
	check := &run.InventoryCheck{AnsibleCore: version, InputDigest: comp.Resolution.InputDigest}
	var diffs []string
	for n, in := range natives {
		seen, err := inventory.ParseListing(outs[n])
		if err != nil {
			return fmt.Errorf("%w: input %s: %w", inventory.ErrResolve, in.inv.Name, err)
		}
		for _, line := range inventory.DiffListings(in.listing, seen) {
			diffs = append(diffs, mask(fmt.Sprintf("input %s: %s", in.inv.Name, line)))
		}
	}
	rendered, err := inventory.ParseListing(outs[len(natives)])
	if err != nil {
		return fmt.Errorf("%w: the rendered inventory: %w", inventory.ErrResolve, err)
	}
	for _, line := range inventory.DiffListings(comp.listing, rendered) {
		diffs = append(diffs, mask("the inventory handed to the play: "+line))
	}
	if check.ResolvedDigest, err = rendered.Digest(); err != nil {
		return fmt.Errorf("%w: %w", inventory.ErrResolve, err)
	}
	r.InventoryCheck = check
	if len(diffs) > 0 {
		check.Differences = diffs
		return fmt.Errorf("%w: ansible-core %s and the native engine read this run's inventory "+
			"differently, so the run is refused rather than executed against hosts or variables the "+
			"evidence does not describe: %s", inventory.ErrDisagreement, version,
			strings.Join(diffs, "; "))
	}
	return nil
}
