package dispatch

import (
	"context"
	"fmt"
	"os"

	"github.com/dcadolph/yardmaster/internal/inventory"
	"github.com/dcadolph/yardmaster/internal/roundhouse"
	"github.com/dcadolph/yardmaster/internal/run"
	"github.com/dcadolph/yardmaster/internal/secretsource"
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

// materializeInventory writes the run's stored inventory to a file for the executor and points
// the spec at it. The returned cleanup removes the file.
func (d *Dispatcher) materializeInventory(r *run.Run, spec *roundhouse.Spec) (func(), error) {
	cleanup := func() {}
	if r.InventoryID == "" {
		return cleanup, nil
	}
	path, remove, err := d.inventoryFile(r.InventoryID)
	if err != nil {
		return cleanup, err
	}
	spec.Inventory = path
	return remove, nil
}

// inventoryFile materializes a stored inventory to a temp file and returns its path and cleanup.
func (d *Dispatcher) inventoryFile(id string) (string, func(), error) {
	if d.inventories == nil {
		return "", func() {}, inventory.ErrNotFound
	}
	inv, err := d.inventories.Get(context.Background(), id)
	if err != nil {
		return "", func() {}, fmt.Errorf("inventory %s: %w", id, err)
	}
	content, err := d.inventoryContent(inv)
	if err != nil {
		return "", func() {}, err
	}
	f, err := os.CreateTemp("", "yardmaster-inventory-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("materialize inventory %s: %w", id, err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", func() {}, fmt.Errorf("materialize inventory %s: %w", id, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", func() {}, fmt.Errorf("materialize inventory %s: %w", id, err)
	}
	return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
}

// inventoryContent returns the inventory's content, resolving it from its content source when that
// source is not local. A non-local source's config is sealed, so it is decrypted and then resolved
// through the shared secretsource engine, letting the host list live in Vault, Google Secret Manager,
// or behind a command rather than in Yardmaster.
func (d *Dispatcher) inventoryContent(inv *inventory.Inventory) (string, error) {
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
	content, err := secretsource.Resolve(context.Background(), inv.ContentSource, config)
	if err != nil {
		return "", fmt.Errorf("inventory %s resolve content: %w", inv.ID, err)
	}
	return content, nil
}
