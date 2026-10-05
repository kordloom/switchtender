package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/kordloom/switchtender/internal/sqlutil"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/util"
)

// triggerColumns is the shared select list for trigger reads.
const triggerColumns = `id, name, template_id, token_hash, signing_secret, require_signature,
	last_fired_at, created_at, created_by, review_provider, review_api_url, review_repository,
	review_credential_id, review_allow_forks, last_error, last_error_at`

// triggerStore is a trigger.Store backed by the shared SQLite database.
type triggerStore struct {
	// db is the open database handle shared with the run store.
	db *splitDB
}

// Save inserts or replaces the trigger.
func (s *triggerStore) Save(ctx context.Context, t *trigger.Trigger) error {
	const q = `
INSERT INTO triggers (id, name, template_id, token_hash, signing_secret, require_signature,
	last_fired_at, created_at, created_by, review_provider, review_api_url, review_repository,
	review_credential_id, review_allow_forks, last_error, last_error_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	name=excluded.name, template_id=excluded.template_id, token_hash=excluded.token_hash,
	signing_secret=excluded.signing_secret, require_signature=excluded.require_signature,
	last_fired_at=excluded.last_fired_at, created_at=excluded.created_at,
	created_by=excluded.created_by, review_provider=excluded.review_provider,
	review_api_url=excluded.review_api_url, review_repository=excluded.review_repository,
	review_credential_id=excluded.review_credential_id,
	review_allow_forks=excluded.review_allow_forks, last_error=excluded.last_error,
	last_error_at=excluded.last_error_at`
	var review trigger.Review
	if t.Review != nil {
		review = *t.Review
	}
	_, err := s.db.ExecContext(ctx, q,
		t.ID, t.Name, t.TemplateID, t.TokenHash, t.SigningSecret, sqlutil.BoolToInt(t.RequireSignature),
		sqlutil.NullTime(t.LastFiredAt), sqlutil.FormatTime(t.CreatedAt), t.CreatedBy,
		review.Provider, review.APIURL, review.Repository, review.CredentialID,
		sqlutil.BoolToInt(review.AllowForks), t.LastError, sqlutil.NullTime(t.LastErrorAt))
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
		// review holds the review columns, kept only when a provider is set.
		review trigger.Review
		// forks is the review's fork flag, stored as an integer like every other boolean.
		forks int
		// refused is when the delivery the last error describes arrived, NULL for none.
		refused sql.NullString
	)
	if err := sc.Scan(&t.ID, &t.Name, &t.TemplateID, &t.TokenHash,
		&t.SigningSecret, &require, &fired, &created, &t.CreatedBy, &review.Provider,
		&review.APIURL, &review.Repository, &review.CredentialID, &forks, &t.LastError,
		&refused); err != nil {
		return nil, err
	}
	t.RequireSignature = require != 0
	if review.Provider != "" {
		review.AllowForks = forks != 0
		t.Review = &review
	}
	var err error
	if t.LastFiredAt, err = sqlutil.ParseNullTime(fired); err != nil {
		return nil, err
	}
	if t.LastErrorAt, err = sqlutil.ParseNullTime(refused); err != nil {
		return nil, err
	}
	if t.CreatedAt, err = sqlutil.ParseTime(created); err != nil {
		return nil, err
	}
	return &t, nil
}

// TouchFired stamps the fire time on a trigger that still exists. An UPDATE by id is the whole
// point: it cannot re-insert a row deletion revoked and it cannot touch the token a rotation
// replaced, both of which the fire path's old whole-row Save did.
func (s *triggerStore) TouchFired(ctx context.Context, id string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		"UPDATE triggers SET last_fired_at = ?, last_error = '', last_error_at = NULL WHERE id = ?",
		sqlutil.FormatTime(at), id); err != nil {
		return fmt.Errorf("touch trigger: %w", err)
	}
	return nil
}

// RecordRefusal stamps why a delivery started no run and when it arrived, on a trigger that still
// exists. An UPDATE by id, for the reason TouchFired gives: a refusal racing a deletion must not
// bring the trigger back.
func (s *triggerStore) RecordRefusal(ctx context.Context, id string, at time.Time, reason string) error {
	if _, err := s.db.ExecContext(ctx,
		"UPDATE triggers SET last_error = ?, last_error_at = ? WHERE id = ?",
		util.SafeText(reason), sqlutil.FormatTime(at), id); err != nil {
		return fmt.Errorf("record trigger refusal: %w", err)
	}
	return nil
}
