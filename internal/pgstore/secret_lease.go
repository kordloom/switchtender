package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// secretLeasesSchema holds the records of dynamic secrets minted for claimed runs, part of schema,
// so any replica can revoke one once the claim it was opened under ends. The handle is sealed by
// the caller and the secret itself is never stored. Times are Unix milliseconds, so the sweep's
// comparisons are exact.
const secretLeasesSchema = `
-- Revoke handles of dynamic secrets minted for claimed runs, sealed, oldest first by created_ms.
CREATE TABLE IF NOT EXISTS secret_leases (
	id            TEXT PRIMARY KEY,
	run_id        TEXT NOT NULL DEFAULT '',
	claim_hash    TEXT NOT NULL DEFAULT '',
	credential_id TEXT NOT NULL DEFAULT '',
	kind          TEXT NOT NULL DEFAULT '',
	handle        TEXT NOT NULL DEFAULT '',
	expires_ms    BIGINT NOT NULL DEFAULT 0,
	created_ms    BIGINT NOT NULL DEFAULT 0,
	attempts      INTEGER NOT NULL DEFAULT 0,
	retry_ms      BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_secret_leases_created ON secret_leases(created_ms, id);
`

// SaveSecretLease records l, replacing any record with its id.
func (s *store) SaveSecretLease(ctx context.Context, l *run.SecretLease) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO secret_leases
	(id, run_id, claim_hash, credential_id, kind, handle, expires_ms, created_ms, attempts, retry_ms)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (id) DO UPDATE SET run_id=excluded.run_id, claim_hash=excluded.claim_hash,
	credential_id=excluded.credential_id, kind=excluded.kind, handle=excluded.handle,
	expires_ms=excluded.expires_ms, created_ms=excluded.created_ms, attempts=excluded.attempts,
	retry_ms=excluded.retry_ms`,
		l.ID, l.RunID, l.ClaimHash, l.CredentialID, l.Kind, l.Handle, l.ExpiresAt.UnixMilli(),
		l.CreatedAt.UnixMilli(), l.Attempts, unixMillis(l.RetryAt)); err != nil {
		return fmt.Errorf("save secret lease: %w", err)
	}
	return nil
}

// ListSecretLeases returns up to limit records, the oldest first.
func (s *store) ListSecretLeases(ctx context.Context, limit int) ([]*run.SecretLease, error) {
	query := `SELECT id, run_id, claim_hash, credential_id, kind, handle, expires_ms, created_ms,
	attempts, retry_ms
FROM secret_leases ORDER BY created_ms, id COLLATE "C"`
	args := []any{}
	if limit > 0 {
		query += " LIMIT $1"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list secret leases: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*run.SecretLease{}
	for rows.Next() {
		var l run.SecretLease
		var expires, created, retry int64
		if err := rows.Scan(&l.ID, &l.RunID, &l.ClaimHash, &l.CredentialID, &l.Kind, &l.Handle,
			&expires, &created, &l.Attempts, &retry); err != nil {
			return nil, fmt.Errorf("list secret leases: %w", err)
		}
		l.ExpiresAt, l.CreatedAt = time.UnixMilli(expires), time.UnixMilli(created)
		if retry > 0 {
			l.RetryAt = time.UnixMilli(retry)
		}
		out = append(out, &l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list secret leases: %w", err)
	}
	return out, nil
}

// unixMillis returns t in Unix milliseconds, and zero for the zero time.
func unixMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// TakeSecretLease deletes the record with id and reports whether this call deleted it.
func (s *store) TakeSecretLease(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM secret_leases WHERE id=$1", id)
	if err != nil {
		return false, fmt.Errorf("take secret lease: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("take secret lease: %w", err)
	}
	return n == 1, nil
}
