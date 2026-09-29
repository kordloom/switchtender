package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// credentialColumns is the shared select list for credential reads.
const credentialColumns = `id, name, kind, secret, created_at, source, org_id, type_id, vault_id,
	settings`

// credentialStore is a credential.Store backed by the shared SQLite database.
type credentialStore struct {
	// db is the open database handle shared with the run store.
	db *sql.DB
}

// Save inserts or replaces the credential.
func (s *credentialStore) Save(ctx context.Context, c *credential.Credential) error {
	settings, err := credential.EncodeSettings(c.Settings)
	if err != nil {
		return fmt.Errorf("save credential: %w", err)
	}
	const q = `
INSERT INTO credentials (id, name, kind, secret, created_at, source, org_id, type_id, vault_id,
	settings)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT(id) DO UPDATE SET
	name=excluded.name, kind=excluded.kind, secret=excluded.secret,
	created_at=excluded.created_at, source=excluded.source, org_id=excluded.org_id,
	type_id=excluded.type_id, vault_id=excluded.vault_id, settings=excluded.settings`
	_, err = s.db.ExecContext(ctx, q,
		c.ID, c.Name, string(c.Kind), c.Secret, sqlutil.FormatTime(c.CreatedAt), c.Source, c.OrgID,
		c.TypeID, c.VaultID, settings)
	if err != nil {
		return fmt.Errorf("save credential: %w", err)
	}
	return nil
}

// Update changes an existing credential's name, kind, and sealed secret, or returns
// credential.ErrNotFound.
func (s *credentialStore) Update(ctx context.Context, c *credential.Credential) error {
	settings, err := credential.EncodeSettings(c.Settings)
	if err != nil {
		return fmt.Errorf("update credential: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		"UPDATE credentials SET name=$1, kind=$2, secret=$3, source=$4, org_id=$5, type_id=$6, "+
			"vault_id=$7, settings=$8 WHERE id=$9",
		c.Name, string(c.Kind), c.Secret, c.Source, c.OrgID, c.TypeID, c.VaultID, settings, c.ID)
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
	const q = "SELECT " + credentialColumns + " FROM credentials WHERE id=$1"
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
	res, err := s.db.ExecContext(ctx, "DELETE FROM credentials WHERE id=$1", id)
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
		c        credential.Credential
		kind     string
		created  string
		settings string
	)
	if err := sc.Scan(&c.ID, &c.Name, &kind, &c.Secret, &created, &c.Source, &c.OrgID, &c.TypeID,
		&c.VaultID, &settings); err != nil {
		return nil, err
	}
	c.Kind = credential.Kind(kind)
	at, err := sqlutil.ParseTime(created)
	if err != nil {
		return nil, err
	}
	c.CreatedAt = at
	if c.Settings, err = credential.DecodeSettings(settings); err != nil {
		return nil, err
	}
	return &c, nil
}
