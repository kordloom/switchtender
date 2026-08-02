package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kordloom/switchtender/internal/credential"
)

// credTypeColumns is the shared select list for credential-type reads.
const credTypeColumns = `id, name, fields, env, extra_vars`

// credTypeStore is a credential.TypeStore backed by the shared SQLite database. A type holds no
// secret, so its fields and injectors are stored as plain JSON columns.
type credTypeStore struct {
	// db is the open database handle shared with the run store.
	db *splitDB
}

// Save inserts or replaces the credential type.
func (s *credTypeStore) Save(ctx context.Context, t *credential.CredentialType) error {
	fields, env, extra, err := marshalType(t)
	if err != nil {
		return err
	}
	const q = `
INSERT INTO credential_types (id, name, fields, env, extra_vars, created_at)
VALUES (?, ?, ?, ?, ?, 0)
ON CONFLICT(id) DO UPDATE SET
	name=excluded.name, fields=excluded.fields, env=excluded.env, extra_vars=excluded.extra_vars`
	if _, err := s.db.ExecContext(ctx, q, t.ID, t.Name, fields, env, extra); err != nil {
		return fmt.Errorf("save credential type: %w", err)
	}
	return nil
}

// Get returns the credential type with the given id, or credential.ErrNotFound.
func (s *credTypeStore) Get(ctx context.Context, id string) (*credential.CredentialType, error) {
	const q = "SELECT " + credTypeColumns + " FROM credential_types WHERE id=?"
	t, err := scanCredType(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, credential.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get credential type: %w", err)
	}
	return t, nil
}

// List returns every credential type ordered by id, oldest first.
func (s *credTypeStore) List(ctx context.Context) ([]*credential.CredentialType, error) {
	const q = "SELECT " + credTypeColumns + " FROM credential_types ORDER BY id"
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list credential types: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*credential.CredentialType
	for rows.Next() {
		t, err := scanCredType(rows)
		if err != nil {
			return nil, fmt.Errorf("list credential types: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list credential types: %w", err)
	}
	return out, nil
}

// Delete removes the credential type with the given id, or returns credential.ErrNotFound.
func (s *credTypeStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM credential_types WHERE id=?", id)
	if err != nil {
		return fmt.Errorf("delete credential type: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete credential type: %w", err)
	}
	if n == 0 {
		return credential.ErrNotFound
	}
	return nil
}

// marshalType encodes a type's fields and injectors as JSON for storage.
func marshalType(t *credential.CredentialType) (fields, env, extra string, err error) {
	fb, err := json.Marshal(t.Fields)
	if err != nil {
		return "", "", "", fmt.Errorf("encode credential type fields: %w", err)
	}
	eb, err := json.Marshal(nonNilMap(t.EnvInjectors))
	if err != nil {
		return "", "", "", fmt.Errorf("encode credential type env: %w", err)
	}
	xb, err := json.Marshal(nonNilMap(t.ExtraVarInjectors))
	if err != nil {
		return "", "", "", fmt.Errorf("encode credential type extra vars: %w", err)
	}
	return string(fb), string(eb), string(xb), nil
}

// nonNilMap returns m, or an empty map so a nil marshals to {} rather than null.
func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// scanCredType reads one credential-type row, decoding its JSON columns.
func scanCredType(sc scanner) (*credential.CredentialType, error) {
	var (
		t                  credential.CredentialType
		fields, env, extra string
	)
	if err := sc.Scan(&t.ID, &t.Name, &fields, &env, &extra); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(fields), &t.Fields); err != nil {
		return nil, fmt.Errorf("decode credential type fields: %w", err)
	}
	if err := json.Unmarshal([]byte(env), &t.EnvInjectors); err != nil {
		return nil, fmt.Errorf("decode credential type env: %w", err)
	}
	if err := json.Unmarshal([]byte(extra), &t.ExtraVarInjectors); err != nil {
		return nil, fmt.Errorf("decode credential type extra vars: %w", err)
	}
	return &t, nil
}
