package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kordloom/switchtender/internal/grant"
)

// grantColumns is the shared select list for grant reads.
const grantColumns = `id, subject, object, access, created_at`

// grantStore is a grant.Store backed by the shared SQLite database.
type grantStore struct {
	// db is the open database handle shared with the run store.
	db *sql.DB
}

// Save inserts or replaces the grant.
func (s *grantStore) Save(ctx context.Context, g *grant.Grant) error {
	const q = `
INSERT INTO grants (id, subject, object, access, created_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	subject=excluded.subject, object=excluded.object, access=excluded.access,
	created_at=excluded.created_at`
	_, err := s.db.ExecContext(ctx, q,
		g.ID, g.Subject, g.Object, string(g.Access), formatTime(g.CreatedAt))
	if err != nil {
		return fmt.Errorf("save grant: %w", err)
	}
	return nil
}

// Get returns the grant with the given id, or grant.ErrNotFound.
func (s *grantStore) Get(ctx context.Context, id string) (*grant.Grant, error) {
	const q = "SELECT " + grantColumns + " FROM grants WHERE id=?"
	g, err := scanGrant(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, grant.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get grant: %w", err)
	}
	return g, nil
}

// List returns all grants ordered by creation time, oldest first.
func (s *grantStore) List(ctx context.Context) ([]*grant.Grant, error) {
	const q = "SELECT " + grantColumns + " FROM grants ORDER BY created_at, id"
	return s.query(ctx, q)
}

// Delete removes the grant with the given id, or returns grant.ErrNotFound.
func (s *grantStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM grants WHERE id=?", id)
	if err != nil {
		return fmt.Errorf("delete grant: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete grant: %w", err)
	}
	if n == 0 {
		return grant.ErrNotFound
	}
	return nil
}

// ForObject returns every grant on the given object, oldest first.
func (s *grantStore) ForObject(ctx context.Context, object string) ([]*grant.Grant, error) {
	const q = "SELECT " + grantColumns + " FROM grants WHERE object=? ORDER BY created_at, id"
	return s.query(ctx, q, object)
}

// query runs a grant select and collects the rows.
func (s *grantStore) query(ctx context.Context, q string, args ...any) ([]*grant.Grant, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query grants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*grant.Grant
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, fmt.Errorf("query grants: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query grants: %w", err)
	}
	return out, nil
}

// scanGrant reads one grant row from a scanner.
func scanGrant(sc scanner) (*grant.Grant, error) {
	var (
		g       grant.Grant
		access  string
		created string
	)
	if err := sc.Scan(&g.ID, &g.Subject, &g.Object, &access, &created); err != nil {
		return nil, err
	}
	g.Access = grant.Access(access)
	at, err := parseTime(created)
	if err != nil {
		return nil, err
	}
	g.CreatedAt = at
	return &g, nil
}
