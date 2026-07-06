package dispatch

import (
	"context"
	"fmt"
	"os"

	"github.com/dcadolph/yardmaster/internal/inventory"
	"github.com/dcadolph/yardmaster/internal/roundhouse"
	"github.com/dcadolph/yardmaster/internal/run"
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
	f, err := os.CreateTemp("", "yardmaster-inventory-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("materialize inventory %s: %w", id, err)
	}
	if _, err := f.WriteString(inv.Content); err != nil {
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
