package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kordloom/switchtender/internal/invsource"
)

// invSourceColumns is the shared select list for inventory source reads.
const invSourceColumns = `id, name, source, credential_id, project_id, inventory_id,
	synced_at, last_error, update_on_launch, sync_interval_seconds, created_at`

// invSourceStore is an invsource.Store backed by the shared SQLite database.
type invSourceStore struct {
	// db is the open database handle shared with the run store.
	db *sql.DB
}

// Save inserts or replaces the source.
func (s *invSourceStore) Save(ctx context.Context, src *invsource.Source) error {
	const q = `
INSERT INTO inventory_sources
	(id, name, source, credential_id, project_id, inventory_id, synced_at, last_error,
	 update_on_launch, sync_interval_seconds, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT(id) DO UPDATE SET
	name=excluded.name, source=excluded.source, credential_id=excluded.credential_id,
	project_id=excluded.project_id, inventory_id=excluded.inventory_id,
	synced_at=excluded.synced_at, last_error=excluded.last_error,
	update_on_launch=excluded.update_on_launch, sync_interval_seconds=excluded.sync_interval_seconds,
	created_at=excluded.created_at`
	_, err := s.db.ExecContext(ctx, q,
		src.ID, src.Name, src.Source, src.CredentialID, src.ProjectID, src.InventoryID,
		nullTime(src.SyncedAt), src.LastError,
		boolToInt(src.UpdateOnLaunch), src.SyncIntervalSeconds, formatTime(src.CreatedAt))
	if err != nil {
		return fmt.Errorf("save inventory source: %w", err)
	}
	return nil
}

// Update changes an existing source's editable fields, leaving its backing inventory and sync
// state intact, or returns invsource.ErrNotFound.
func (s *invSourceStore) Update(ctx context.Context, src *invsource.Source) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE inventory_sources SET name=$1, source=$2, credential_id=$3, project_id=$4, "+
			"update_on_launch=$5, sync_interval_seconds=$6 WHERE id=$7",
		src.Name, src.Source, src.CredentialID, src.ProjectID,
		boolToInt(src.UpdateOnLaunch), src.SyncIntervalSeconds, src.ID)
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
	const q = "SELECT " + invSourceColumns + " FROM inventory_sources WHERE id=$1"
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
	res, err := s.db.ExecContext(ctx, "DELETE FROM inventory_sources WHERE id=$1", id)
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
		src      invsource.Source
		synced   sql.NullString
		onLaunch int
		created  string
	)
	if err := sc.Scan(&src.ID, &src.Name, &src.Source, &src.CredentialID, &src.ProjectID,
		&src.InventoryID, &synced, &src.LastError, &onLaunch, &src.SyncIntervalSeconds, &created); err != nil {
		return nil, err
	}
	src.UpdateOnLaunch = onLaunch != 0
	var err error
	if src.SyncedAt, err = parseNullTime(synced); err != nil {
		return nil, err
	}
	if src.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	return &src, nil
}
