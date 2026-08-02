package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// credentialColumns is the shared select list for credential reads.
const credentialColumns = `id, name, kind, secret, created_at, source, org_id, type_id`

// credentialStore is a credential.Store backed by the shared SQLite database.
type credentialStore struct {
	// db is the open database handle shared with the run store.
	db *splitDB
}

// Save inserts or replaces the credential.
func (s *credentialStore) Save(ctx context.Context, c *credential.Credential) error {
	const q = `
INSERT INTO credentials (id, name, kind, secret, created_at, source, org_id, type_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	name=excluded.name, kind=excluded.kind, secret=excluded.secret,
	created_at=excluded.created_at, source=excluded.source, org_id=excluded.org_id,
	type_id=excluded.type_id`
	_, err := s.db.ExecContext(ctx, q,
		c.ID, c.Name, string(c.Kind), c.Secret, sqlutil.FormatTime(c.CreatedAt), c.Source, c.OrgID,
		c.TypeID)
	if err != nil {
		return fmt.Errorf("save credential: %w", err)
	}
	return nil
}

// Update changes an existing credential's name, kind, and sealed secret, or returns
// credential.ErrNotFound.
func (s *credentialStore) Update(ctx context.Context, c *credential.Credential) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE credentials SET name=?, kind=?, secret=?, source=?, org_id=?, type_id=? WHERE id=?",
		c.Name, string(c.Kind), c.Secret, c.Source, c.OrgID, c.TypeID, c.ID)
	if err != nil {
		return fmt.Errorf("update credential: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update credential: %w", err)
	}
	if n == 0 {
		return credential.ErrNotFound
	}
	return nil
}

// Get returns the credential with the given id, or credential.ErrNotFound.
func (s *credentialStore) Get(ctx context.Context, id string) (*credential.Credential, error) {
	const q = "SELECT " + credentialColumns + " FROM credentials WHERE id=?"
	c, err := scanCredential(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, credential.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get credential: %w", err)
	}
	return c, nil
}

// List returns all credentials ordered by creation time, oldest first.
func (s *credentialStore) List(ctx context.Context) ([]*credential.Credential, error) {
	const q = "SELECT " + credentialColumns + " FROM credentials ORDER BY created_at, id"
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*credential.Credential
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, fmt.Errorf("list credentials: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}
	return out, nil
}

// Delete removes the credential with the given id, or returns credential.ErrNotFound.
func (s *credentialStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM credentials WHERE id=?", id)
	if err != nil {
		return fmt.Errorf("delete credential: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete credential: %w", err)
	}
	if n == 0 {
		return credential.ErrNotFound
	}
	return nil
}

// scanCredential reads one credential row from a scanner.
func scanCredential(sc scanner) (*credential.Credential, error) {
	var (
		c       credential.Credential
		kind    string
		created string
	)
	if err := sc.Scan(&c.ID, &c.Name, &kind, &c.Secret, &created, &c.Source, &c.OrgID, &c.TypeID); err != nil {
		return nil, err
	}
	c.Kind = credential.Kind(kind)
	at, err := sqlutil.ParseTime(created)
	if err != nil {
		return nil, err
	}
	c.CreatedAt = at
	return &c, nil
}
