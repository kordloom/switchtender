package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/dcadolph/yardmaster/internal/inventory"
)

// inventoryColumns is the shared select list for inventory reads.
const inventoryColumns = `id, name, content, created_at`

// inventoryStore is an inventory.Store backed by the shared SQLite database.
type inventoryStore struct {
	// db is the open database handle shared with the run store.
	db *sql.DB
}

// Save inserts or replaces the inventory.
func (s *inventoryStore) Save(ctx context.Context, i *inventory.Inventory) error {
	const q = `
INSERT INTO inventories (id, name, content, created_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT(id) DO UPDATE SET
	name=excluded.name, content=excluded.content, created_at=excluded.created_at`
	_, err := s.db.ExecContext(ctx, q, i.ID, i.Name, i.Content, formatTime(i.CreatedAt))
	if err != nil {
		return fmt.Errorf("save inventory: %w", err)
	}
	return nil
}

// Get returns the inventory with the given id, or inventory.ErrNotFound.
func (s *inventoryStore) Get(ctx context.Context, id string) (*inventory.Inventory, error) {
	const q = "SELECT " + inventoryColumns + " FROM inventories WHERE id=$1"
	i, err := scanInventory(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, inventory.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get inventory: %w", err)
	}
	return i, nil
}

// List returns all inventories ordered by creation time, oldest first.
func (s *inventoryStore) List(ctx context.Context) ([]*inventory.Inventory, error) {
	const q = "SELECT " + inventoryColumns + " FROM inventories ORDER BY created_at, id"
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list inventories: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*inventory.Inventory
	for rows.Next() {
		i, err := scanInventory(rows)
		if err != nil {
			return nil, fmt.Errorf("list inventories: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list inventories: %w", err)
	}
	return out, nil
}

// Delete removes the inventory with the given id, or returns inventory.ErrNotFound.
func (s *inventoryStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM inventories WHERE id=$1", id)
	if err != nil {
		return fmt.Errorf("delete inventory: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete inventory: %w", err)
	}
	if n == 0 {
		return inventory.ErrNotFound
	}
	return nil
}

// scanInventory reads one inventory row from a scanner.
func scanInventory(sc scanner) (*inventory.Inventory, error) {
	var (
		i       inventory.Inventory
		created string
	)
	if err := sc.Scan(&i.ID, &i.Name, &i.Content, &created); err != nil {
		return nil, err
	}
	at, err := parseTime(created)
	if err != nil {
		return nil, err
	}
	i.CreatedAt = at
	return &i, nil
}
