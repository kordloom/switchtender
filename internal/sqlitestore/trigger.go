package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/dcadolph/railwarden/internal/trigger"
)

// triggerColumns is the shared select list for trigger reads.
const triggerColumns = `id, name, template_id, token_hash, signing_secret, require_signature, last_fired_at, created_at`

// triggerStore is a trigger.Store backed by the shared SQLite database.
type triggerStore struct {
	// db is the open database handle shared with the run store.
	db *sql.DB
}

// Save inserts or replaces the trigger.
func (s *triggerStore) Save(ctx context.Context, t *trigger.Trigger) error {
	const q = `
INSERT INTO triggers (id, name, template_id, token_hash, signing_secret, require_signature, last_fired_at, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	name=excluded.name, template_id=excluded.template_id, token_hash=excluded.token_hash,
	signing_secret=excluded.signing_secret, require_signature=excluded.require_signature,
	last_fired_at=excluded.last_fired_at, created_at=excluded.created_at`
	_, err := s.db.ExecContext(ctx, q,
		t.ID, t.Name, t.TemplateID, t.TokenHash, t.SigningSecret, boolToInt(t.RequireSignature),
		nullTime(t.LastFiredAt), formatTime(t.CreatedAt))
	if err != nil {
		return fmt.Errorf("save trigger: %w", err)
	}
	return nil
}

// Get returns the trigger with the given id, or trigger.ErrNotFound.
func (s *triggerStore) Get(ctx context.Context, id string) (*trigger.Trigger, error) {
	const q = "SELECT " + triggerColumns + " FROM triggers WHERE id=?"
	t, err := scanTrigger(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, trigger.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get trigger: %w", err)
	}
	return t, nil
}

// List returns all triggers ordered by creation time, oldest first.
func (s *triggerStore) List(ctx context.Context) ([]*trigger.Trigger, error) {
	const q = "SELECT " + triggerColumns + " FROM triggers ORDER BY created_at, id"
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list triggers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*trigger.Trigger
	for rows.Next() {
		t, err := scanTrigger(rows)
		if err != nil {
			return nil, fmt.Errorf("list triggers: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list triggers: %w", err)
	}
	return out, nil
}

// Delete removes the trigger with the given id, or returns trigger.ErrNotFound.
func (s *triggerStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM triggers WHERE id=?", id)
	if err != nil {
		return fmt.Errorf("delete trigger: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete trigger: %w", err)
	}
	if n == 0 {
		return trigger.ErrNotFound
	}
	return nil
}

// FindByTokenHash returns the trigger with the given token hash, or trigger.ErrNotFound.
func (s *triggerStore) FindByTokenHash(ctx context.Context, hash string) (*trigger.Trigger, error) {
	const q = "SELECT " + triggerColumns + " FROM triggers WHERE token_hash=?"
	t, err := scanTrigger(s.db.QueryRowContext(ctx, q, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, trigger.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find trigger: %w", err)
	}
	return t, nil
}

// scanTrigger reads one trigger row from a scanner.
func scanTrigger(sc scanner) (*trigger.Trigger, error) {
	var (
		t       trigger.Trigger
		require int
		fired   sql.NullString
		created string
	)
	if err := sc.Scan(&t.ID, &t.Name, &t.TemplateID, &t.TokenHash,
		&t.SigningSecret, &require, &fired, &created); err != nil {
		return nil, err
	}
	t.RequireSignature = require != 0
	var err error
	if t.LastFiredAt, err = parseNullTime(fired); err != nil {
		return nil, err
	}
	if t.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	return &t, nil
}
