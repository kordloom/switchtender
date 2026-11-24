package dispatch

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/kordloom/switchtender/internal/run"
)

// inventoryScanner reads the inventory an Ansible dry run of r executes against.
type inventoryScanner func(r *run.Run, checkouts CheckoutReader) run.InventoryScan

// scanInventory reads the inventory an Ansible dry run of r executes against, the way the gate
// reads it: a stored inventory through the snapshot r executes, and an inventory named by path from
// the project at the commit r executes, or from this server.
func (d *Dispatcher) scanInventory(r *run.Run, checkouts CheckoutReader) run.InventoryScan {
	return d.scanInventoryAt(context.Background(), r, checkouts)
}

// scanInventoryAt is scanInventory with the context a stored inventory is read under when r carries
// no snapshot yet.
func (d *Dispatcher) scanInventoryAt(ctx context.Context, r *run.Run,
	checkouts CheckoutReader) run.InventoryScan {
	if r.InventoryID != "" {
		return d.scanStoredInventory(ctx, r)
	}
	return scanPathInventory(r, checkouts)
}

// localInventory reads an inventory named by path, and records a stored inventory as unread, since
// its snapshot opens only through the dispatcher that sealed it. It serves the plan gate's free
// functions, which grade without a dispatcher.
func localInventory(r *run.Run, checkouts CheckoutReader) run.InventoryScan {
	if r.InventoryID != "" {
		return run.UnreadStoredInventory(r.InventoryID, "its snapshot opens only on the server "+
			"that sealed it")
	}
	return scanPathInventory(r, checkouts)
}

// scanStoredInventory reads the stored inventory r targets as the run executes it: the snapshot r
// was submitted with, the composed result for a composed inventory. A run that carries no snapshot
// yet, the run a pull request review asks about before it plans, is read as a submission would
// snapshot it now.
func (d *Dispatcher) scanStoredInventory(ctx context.Context, r *run.Run) run.InventoryScan {
	name := r.InventoryID
	if d.inventories != nil {
		if inv, err := d.inventories.Get(ctx, r.InventoryID); err == nil {
			name = inv.Name
		}
	}
	if r.InventorySealed != "" {
		content, err := d.openSnapshot(r)
		if err != nil {
			return run.UnreadStoredInventory(name, "its snapshot could not be opened")
		}
		play, err := snapshotPlay(r, content)
		if err != nil {
			return run.UnreadStoredInventory(name, "its composed snapshot could not be read")
		}
		return run.ScanStoredInventory(play, name)
	}
	if d.inventories == nil {
		return run.UnreadStoredInventory(name, "no inventory store is on this server")
	}
	inv, err := d.inventories.Get(ctx, r.InventoryID)
	if err != nil {
		return run.UnreadStoredInventory(name, "it could not be read")
	}
	if inv.Composed() {
		c, err := d.compose(ctx, inv, run.InventoryAccessFrom(ctx), nil)
		if err != nil {
			return run.UnreadStoredInventory(name, "it could not be composed")
		}
		return run.ScanStoredInventory(c.Content, name)
	}
	content, err := d.inventoryContent(ctx, inv)
	if err != nil {
		return run.UnreadStoredInventory(name, "its content could not be read")
	}
	return run.ScanStoredInventory(content, name)
}

// scanPathInventory reads an inventory r names by path: inside the project at the commit that
// decides what r executes, or on this server. It reads nothing for a run naming no inventory.
func scanPathInventory(r *run.Run, checkouts CheckoutReader) run.InventoryScan {
	if r.Inventory == "" || r.InventoryID != "" {
		return run.InventoryScan{}
	}
	if r.ProjectID != "" {
		return scanProjectInventory(r, checkouts)
	}
	return scanLocalInventory(r.Inventory)
}

// scanProjectInventory reads a project run's inventory at the commit the playbook scan reads.
func scanProjectInventory(r *run.Run, checkouts CheckoutReader) run.InventoryScan {
	if checkouts == nil {
		return run.UnreadInventoryFile(r.Inventory, noCheckout)
	}
	commit, _ := commitToRead(r)
	var out run.InventoryScan
	read := false
	err := checkouts.ReadCommit(r.ProjectID, commit, func(fsys fs.FS, _ string) error {
		out = run.ScanInventoryIn(fsys, r.Inventory, "the project")
		read = true
		return nil
	})
	if err != nil || !read {
		return run.UnreadInventoryFile(r.Inventory, "the project's checkout could not be read")
	}
	return out
}

// scanLocalInventory reads an inventory named by a path on this server. The directory holding it
// bounds what the scan reads, and the inventory itself is not followed if it is a link, so a link
// is reported as one.
func scanLocalInventory(name string) run.InventoryScan {
	abs, err := filepath.Abs(name)
	if err != nil {
		return run.UnreadInventoryFile(name, "it could not be read")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && strings.Contains(name, ",") {
			return run.HostListScan(name)
		}
		return run.UnreadInventoryFile(name, unreadWhy(err, "not on this server"))
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return run.UnreadInventoryFile(name, "it could not be read")
	}
	defer func() { _ = root.Close() }()
	return run.ScanInventoryIn(root.FS(), filepath.Base(abs), "the inventory's directory")
}
