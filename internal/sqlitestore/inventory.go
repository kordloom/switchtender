package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// inventoryColumns is the shared select list for inventory reads.
const inventoryColumns = `id, name, content, credential_ids, content_source, content_config, queue,
	org_id, created_at, kind, host_filter, input_ids, source_vars, limit_pattern`

// inventoryStore is an inventory.Store backed by the shared SQLite database.
type inventoryStore struct {
	// db is the open database handle shared with the run store.
	db *splitDB
}

// Save inserts or replaces the inventory.
func (s *inventoryStore) Save(ctx context.Context, i *inventory.Inventory) error {
	const q = `
INSERT INTO inventories (id, name, content, credential_ids, content_source, content_config, queue,
	org_id, created_at, kind, host_filter, input_ids, source_vars, limit_pattern)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	name=excluded.name, content=excluded.content, credential_ids=excluded.credential_ids,
	content_source=excluded.content_source, content_config=excluded.content_config,
	queue=excluded.queue, org_id=excluded.org_id, created_at=excluded.created_at,
	kind=excluded.kind, host_filter=excluded.host_filter, input_ids=excluded.input_ids,
	source_vars=excluded.source_vars, limit_pattern=excluded.limit_pattern`
	_, err := s.db.ExecContext(ctx, q,
		i.ID, i.Name, i.Content, sqlutil.JoinIDs(i.CredentialIDs), i.ContentSource, i.ContentConfig, i.Queue,
		i.OrgID, sqlutil.FormatTime(i.CreatedAt), i.Kind, i.HostFilter, sqlutil.JoinIDs(i.InputIDs),
		i.SourceVars, i.Limit)
	if err != nil {
		return fmt.Errorf("save inventory: %w", err)
	}
	return nil
}

// Update changes an existing inventory's name and content, or returns inventory.ErrNotFound.
func (s *inventoryStore) Update(ctx context.Context, i *inventory.Inventory) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE inventories SET name=?, content=?, credential_ids=?, content_source=?, "+
			"content_config=?, queue=?, org_id=?, kind=?, host_filter=?, input_ids=?, source_vars=?, "+
			"limit_pattern=? WHERE id=?",
		i.Name, i.Content, sqlutil.JoinIDs(i.CredentialIDs), i.ContentSource, i.ContentConfig, i.Queue,
		i.OrgID, i.Kind, i.HostFilter, sqlutil.JoinIDs(i.InputIDs), i.SourceVars, i.Limit, i.ID)
	if err != nil {
		return fmt.Errorf("update inventory: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update inventory: %w", err)
	}
	if n == 0 {
		return inventory.ErrNotFound
	}
	return nil
}

// Get returns the inventory with the given id, or inventory.ErrNotFound.
func (s *inventoryStore) Get(ctx context.Context, id string) (*inventory.Inventory, error) {
	const q = "SELECT " + inventoryColumns + " FROM inventories WHERE id=?"
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
	// The inventory and the facts cached for its hosts go in one transaction. Facts left behind
	// would be served to a later inventory that reused the id, and they are the inventory's data:
	// deleting the host list and keeping what every host reported about itself is not a delete.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete inventory: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, "DELETE FROM inventories WHERE id=?", id)
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
	const facts = "DELETE FROM host_fact_cache WHERE inventory_id=?"
	if _, err := tx.ExecContext(ctx, facts, id); err != nil {
		return fmt.Errorf("delete inventory cached facts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete inventory: %w", err)
	}
	return nil
}

// scanInventory reads one inventory row from a scanner.
func scanInventory(sc scanner) (*inventory.Inventory, error) {
	var (
		i       inventory.Inventory
		creds   string
		created string
		inputs  string
	)
	if err := sc.Scan(&i.ID, &i.Name, &i.Content, &creds, &i.ContentSource, &i.ContentConfig,
		&i.Queue, &i.OrgID, &created, &i.Kind, &i.HostFilter, &inputs, &i.SourceVars,
		&i.Limit); err != nil {
		return nil, err
	}
	i.CredentialIDs = sqlutil.SplitIDs(creds)
	i.InputIDs = sqlutil.SplitIDs(inputs)
	at, err := sqlutil.ParseTime(created)
	if err != nil {
		return nil, err
	}
	i.CreatedAt = at
	return &i, nil
}
