package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/dcadolph/railwarden/internal/auth"
)

// tokenColumns is the shared select list for token reads.
const tokenColumns = `id, name, hash, user_id, created_at, last_used_at, expires_at`

// tokenStore is an auth.Store backed by the shared SQLite database.
type tokenStore struct {
	// db is the open database handle shared with the run store.
	db *sql.DB
}

// Save inserts or replaces the token.
func (s *tokenStore) Save(ctx context.Context, t *auth.Token) error {
	const q = `
INSERT INTO tokens (id, name, hash, user_id, created_at, last_used_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT(id) DO UPDATE SET
	name=excluded.name, hash=excluded.hash, user_id=excluded.user_id,
	created_at=excluded.created_at, last_used_at=excluded.last_used_at,
	expires_at=excluded.expires_at`
	_, err := s.db.ExecContext(ctx, q,
		t.ID, t.Name, t.Hash, t.UserID, formatTime(t.CreatedAt), nullTime(t.LastUsedAt),
		nullTime(t.ExpiresAt))
	if err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	return nil
}

// List returns all tokens ordered by creation time, oldest first.
func (s *tokenStore) List(ctx context.Context) ([]*auth.Token, error) {
	const q = "SELECT " + tokenColumns + " FROM tokens ORDER BY created_at, id"
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*auth.Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, fmt.Errorf("list tokens: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	return out, nil
}

// Delete removes the token with the given id, or returns auth.ErrNotFound.
func (s *tokenStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM tokens WHERE id=$1", id)
	if err != nil {
		return fmt.Errorf("delete token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete token: %w", err)
	}
	if n == 0 {
		return auth.ErrNotFound
	}
	return nil
}

// FindByHash returns the token with the given hash, or auth.ErrNotFound.
func (s *tokenStore) FindByHash(ctx context.Context, hash string) (*auth.Token, error) {
	const q = "SELECT " + tokenColumns + " FROM tokens WHERE hash=$1"
	t, err := scanToken(s.db.QueryRowContext(ctx, q, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, auth.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find token: %w", err)
	}
	return t, nil
}

// Count returns how many tokens exist.
func (s *tokenStore) Count(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tokens").Scan(&n); err != nil {
		return 0, fmt.Errorf("count tokens: %w", err)
	}
	return n, nil
}

// scanToken reads one token row from a scanner.
func scanToken(sc scanner) (*auth.Token, error) {
	var (
		t        auth.Token
		created  string
		lastUsed sql.NullString
		expires  sql.NullString
	)
	if err := sc.Scan(&t.ID, &t.Name, &t.Hash, &t.UserID, &created, &lastUsed, &expires); err != nil {
		return nil, err
	}
	at, err := parseTime(created)
	if err != nil {
		return nil, err
	}
	t.CreatedAt = at
	if t.LastUsedAt, err = parseNullTime(lastUsed); err != nil {
		return nil, err
	}
	if t.ExpiresAt, err = parseNullTime(expires); err != nil {
		return nil, err
	}
	return &t, nil
}
