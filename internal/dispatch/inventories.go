package dispatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/runfiles"
	"github.com/kordloom/switchtender/internal/secretsource"
)

// WithInventories lets runs target stored inventories by id.
func WithInventories(store inventory.Store) Option {
	return func(c *config) { c.inventories = store }
}

// validateInventory confirms a referenced inventory exists before a run is accepted.
func (d *Dispatcher) validateInventory(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	if d.inventories == nil {
		return inventory.ErrNotFound
	}
	if _, err := d.inventories.Get(ctx, id); err != nil {
		return fmt.Errorf("%w: %s", err, id)
	}
	return nil
}

// materializeInventory writes the run's stored inventory to a file for the executor and points the
// spec at it. It returns the cleanup that removes the file, the secret-looking values in the host
// list, so a variable such as ansible_password is masked in the run's output, and for a composed
// inventory what it resolved to, which the cross-check before an Ansible run reads.
//
// A plain stored inventory is the snapshot the run was submitted with, opened through src, never the
// inventory as the store holds it now. A composed inventory is rendered from its inputs held to the
// hosts it resolved to at submission. A run naming a stored inventory with neither is refused: it
// would otherwise execute whatever the inventory holds when it is claimed, which nobody approved.
func (d *Dispatcher) materializeInventory(ctx context.Context, src secretSource, r *run.Run,
	spec *roundhouse.Spec) (func(), []string, *composed, error) {
	cleanup := func() {}
	if r.InventoryID == "" {
		return cleanup, nil, nil, nil
	}
	if r.InventorySnapshot != nil {
		content, err := src.inventorySnapshot(ctx, r)
		if err != nil {
			return cleanup, nil, nil, err
		}
		remove, secrets, err := d.materializeSnapshot(r, content, spec)
		return remove, secrets, nil, err
	}
	if r.InventoryResolution == nil {
		return cleanup, nil, nil, fmt.Errorf("%w: this run names stored inventory %s and carries no "+
			"snapshot of it, so it is refused rather than run against whatever the inventory holds "+
			"now. Submit it again", ErrInventorySnapshot, r.InventoryID)
	}
	path, remove, secrets, comp, err := d.inventoryFile(ctx, r.InventoryID, r.InventoryResolution)
	if err != nil {
		return cleanup, nil, nil, err
	}
	spec.Inventory = path
	return remove, secrets, comp, nil
}

// inventoryFile materializes a stored inventory to a file in a directory of its own and returns its
// path, cleanup, the secret-looking values in its content, and for a composed inventory what it
// resolved to. A smart or constructed inventory is rendered from its inputs, held to res, the
// resolution the run recorded when it launched.
//
// The directory is private to this run. Ansible reads group_vars and host_vars directories beside
// an inventory file, so a file written straight into the shared temporary directory took whatever
// variables another account had left there, and neither the record nor the cross-check would have
// known. A host list can carry secret variables, such as ansible_password, so the directory is a run
// directory, staged the way a credential is: locked and counted while in use and swept if the
// process dies, where a crash in the shared temporary directory left it until the operating system
// cleared that.
func (d *Dispatcher) inventoryFile(ctx context.Context, id string,
	res *run.InventoryResolution) (string, func(), []string, *composed, error) {
	noop := func() {}
	if d.inventories == nil {
		return "", noop, nil, nil, inventory.ErrNotFound
	}
	inv, err := d.inventories.Get(ctx, id)
	if err != nil {
		return "", noop, nil, nil, fmt.Errorf("inventory %s: %w", id, err)
	}
	var (
		content string
		comp    *composed
	)
	if inv.Composed() {
		comp, err = d.composedContent(ctx, inv, res)
		if comp != nil {
			content = comp.Content
		}
	} else {
		content, err = d.inventoryContent(ctx, inv)
	}
	if err != nil {
		return "", noop, nil, nil, err
	}
	dir, err := runfiles.Create(d.runFiles(), "inventory-"+id)
	if err != nil {
		return "", noop, nil, nil, fmt.Errorf("materialize inventory %s: %w", id, err)
	}
	remove := func() { _ = dir.Remove() }
	path := filepath.Join(dir.Path(), "inventory")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		remove()
		return "", noop, nil, nil, fmt.Errorf("materialize inventory %s: %w", id, err)
	}
	return path, remove, inventorySecrets(content), comp, nil
}

// inventoryContent returns the inventory's content, resolving it from its content source when that
// source is not local. A non-local source's config is sealed, so it is decrypted and then resolved
// through the shared secretsource engine, letting the host list live in Vault, Google Secret Manager,
// or behind a command rather than in SwitchTender.
func (d *Dispatcher) inventoryContent(ctx context.Context, inv *inventory.Inventory) (string, error) {
	if secretsource.NormalizeKind(inv.ContentSource) == secretsource.KindLocal {
		return inv.Content, nil
	}
	if d.sealer == nil {
		return "", fmt.Errorf("inventory %s content source needs an encryption key", inv.ID)
	}
	config, err := d.sealer.Open(inv.ContentConfig)
	if err != nil {
		return "", fmt.Errorf("inventory %s decrypt content source: %w", inv.ID, err)
	}
	content, err := secretsource.Resolve(ctx, inv.ContentSource, config)
	if err != nil {
		return "", fmt.Errorf("inventory %s resolve content: %w", inv.ID, err)
	}
	return content, nil
}

// inventorySecrets returns the values of secret-looking variables in inventory content, so a host
// list that carries an ansible_password or an API token does not leak it into the run's log or
// events. The detection lives with the inventory type, because the API redacts the same values when
// it serves an inventory and the two must not drift apart.
func inventorySecrets(content string) []string {
	return inventory.Secrets(content)
}
