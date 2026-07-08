package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/dcadolph/yardmaster/internal/invsource"
)

// invSourceColumns is the shared select list for inventory source reads.
const invSourceColumns = `id, name, source, credential_id, project_id, inventory_id,
	synced_at, last_error, created_at`

// invSourceStore is an invsource.Store backed by the shared SQLite database.
type invSourceStore struct {
	// db is the open database handle shared with the run store.
	db *sql.DB
}

// Save inserts or replaces the source.
func (s *invSourceStore) Save(ctx context.Context, src *invsource.Source) error {
	const q = `
INSERT INTO inventory_sources
	(id, name, source, credential_id, project_id, inventory_id, synced_at, last_error, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	name=excluded.name, source=excluded.source, credential_id=excluded.credential_id,
	project_id=excluded.project_id, inventory_id=excluded.inventory_id,
	synced_at=excluded.synced_at, last_error=excluded.last_error, created_at=excluded.created_at`
	_, err := s.db.ExecContext(ctx, q,
		src.ID, src.Name, src.Source, src.CredentialID, src.ProjectID, src.InventoryID,
		nullTime(src.SyncedAt), src.LastError, formatTime(src.CreatedAt))
	if err != nil {
		return fmt.Errorf("save inventory source: %w", err)
	}
	return nil
}

// Update changes an existing source's editable fields, leaving its backing inventory and sync
// state intact, or returns invsource.ErrNotFound.
func (s *invSourceStore) Update(ctx context.Context, src *invsource.Source) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE inventory_sources SET name=?, source=?, credential_id=?, project_id=? WHERE id=?",
		src.Name, src.Source, src.CredentialID, src.ProjectID, src.ID)
	if err != nil {
		return fmt.Errorf("update inventory source: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update inventory source: %w", err)
	}
	if n == 0 {
		return invsource.ErrNotFound
	}
	return nil
}

// Get returns the source with the given id, or invsource.ErrNotFound.
func (s *invSourceStore) Get(ctx context.Context, id string) (*invsource.Source, error) {
	const q = "SELECT " + invSourceColumns + " FROM inventory_sources WHERE id=?"
	src, err := scanInvSource(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, invsource.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get inventory source: %w", err)
	}
	return src, nil
}

// List returns all sources ordered by creation time, oldest first.
func (s *invSourceStore) List(ctx context.Context) ([]*invsource.Source, error) {
	const q = "SELECT " + invSourceColumns + " FROM inventory_sources ORDER BY created_at, id"
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list inventory sources: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*invsource.Source
	for rows.Next() {
		src, err := scanInvSource(rows)
		if err != nil {
			return nil, fmt.Errorf("list inventory sources: %w", err)
		}
		out = append(out, src)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list inventory sources: %w", err)
	}
	return out, nil
}

// Delete removes the source with the given id, or returns invsource.ErrNotFound.
func (s *invSourceStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM inventory_sources WHERE id=?", id)
	if err != nil {
		return fmt.Errorf("delete inventory source: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete inventory source: %w", err)
	}
	if n == 0 {
		return invsource.ErrNotFound
	}
	return nil
}

// scanInvSource reads one source row from a scanner.
func scanInvSource(sc scanner) (*invsource.Source, error) {
	var (
		src     invsource.Source
		synced  sql.NullString
		created string
	)
	if err := sc.Scan(&src.ID, &src.Name, &src.Source, &src.CredentialID, &src.ProjectID,
		&src.InventoryID, &synced, &src.LastError, &created); err != nil {
		return nil, err
	}
	var err error
	if src.SyncedAt, err = parseNullTime(synced); err != nil {
		return nil, err
	}
	if src.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	return &src, nil
}
