package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/kordloom/switchtender/internal/review"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// reviewColumns is the shared select list for review report reads.
const reviewColumns = `id, kind, run_id, trigger_id, pull_request, commit_sha, reason, receipt,
	phase, held_by, status_state, comment_sha256, recorded_sha256, reported_at, done, version,
	claimed_by, claimed_until, attempts, failing_since, retry_at, last_error, created_at, provider,
	api_url, repository, status_context`

// reviewStore is a review.Store backed by the shared SQLite database. Every write goes through the
// single write connection, so a claim's lane check and its update are one serialized step.
type reviewStore struct {
	// db is the open database handle shared with the run store.
	db *splitDB
}

// Create inserts rec unless a record with its id exists.
func (s *reviewStore) Create(ctx context.Context, rec *review.Record) (bool, error) {
	const q = `INSERT INTO review_reports (` + reviewColumns + `)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO NOTHING`
	rec.Sanitize()
	res, err := s.db.ExecContext(ctx, q,
		rec.ID, rec.Kind, rec.RunID, rec.TriggerID, rec.PullRequest, rec.CommitSHA, rec.Reason,
		rec.Receipt, rec.Phase, rec.HeldBy, rec.StatusState, rec.CommentSHA256, rec.RecordedSHA256,
		optionalTime(rec.ReportedAt), sqlutil.BoolToInt(rec.Done), rec.Version, rec.ClaimedBy,
		optionalTime(rec.ClaimedUntil), rec.Attempts, optionalTime(rec.FailingSince),
		optionalTime(rec.RetryAt), rec.LastError, sqlutil.FormatTime(rec.CreatedAt), rec.Provider,
		rec.APIURL, rec.Repository, rec.StatusContext)
	if err != nil {
		return false, fmt.Errorf("create review report: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("create review report: %w", err)
	}
	return n > 0, nil
}

// Get returns the record with id, or review.ErrRecordNotFound.
func (s *reviewStore) Get(ctx context.Context, id string) (*review.Record, error) {
	rec, err := scanReviewRecord(s.db.QueryRowContext(ctx,
		"SELECT "+reviewColumns+" FROM review_reports WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, review.ErrRecordNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get review report: %w", err)
	}
	return rec, nil
}

// Pending returns up to limit records not yet done, oldest first. The stored time is trimmed of its
// zone letter before it is compared, the same fix sqlutil.TimeOrder applies, so a whole second does
// not sort after the fractions that follow it.
func (s *reviewStore) Pending(ctx context.Context, limit int) ([]*review.Record, error) {
	q := "SELECT " + reviewColumns + " FROM review_reports WHERE done=0 " +
		"ORDER BY rtrim(created_at, 'Z'), id"
	args := []any{}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list pending review reports: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*review.Record{}
	for rows.Next() {
		rec, err := scanReviewRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("list pending review reports: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending review reports: %w", err)
	}
	review.SortRecords(out)
	return out, nil
}

// Claim takes the record for owner when its version is still version and no other record of the
// same pull request and trigger holds a claim live at now. The lane check and the update run in one
// transaction on the single write connection, so no other claim can land between them.
func (s *reviewStore) Claim(ctx context.Context, id string, version int64, owner string, now,
	until time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("claim review report: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var triggerID string
	var number int
	err = tx.QueryRowContext(ctx,
		"SELECT trigger_id, pull_request FROM review_reports WHERE id=? AND done=0 AND version=?",
		id, version).Scan(&triggerID, &number)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim review report: %w", err)
	}
	busy, err := laneBusy(ctx, tx, `SELECT claimed_by, claimed_until FROM review_reports
WHERE trigger_id=? AND pull_request=? AND id<>? AND claimed_by<>''`, triggerID, number, id, now)
	if err != nil {
		return false, fmt.Errorf("claim review report: %w", err)
	}
	if busy {
		return false, nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE review_reports SET version=version+1, claimed_by=?,
	claimed_until=? WHERE id=? AND version=? AND done=0`,
		owner, sqlutil.FormatTime(until), id, version)
	if err != nil {
		return false, fmt.Errorf("claim review report: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim review report: %w", err)
	}
	if n == 0 {
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("claim review report: %w", err)
	}
	return true, nil
}

// laneBusy reports whether any row q selects, each a claimant and its lapse time, holds a claim
// still live at now. The times are compared parsed rather than as text, since the stored form trims
// its fraction and text order is wrong inside one second.
func laneBusy(ctx context.Context, tx *sql.Tx, q string, triggerID string, number int, id string,
	now time.Time) (bool, error) {
	rows, err := tx.QueryContext(ctx, q, triggerID, number, id)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	busy := false
	for rows.Next() {
		var by, until string
		if err := rows.Scan(&by, &until); err != nil {
			return false, err
		}
		if t := sqlutil.ParseTimeOrAbsent(until); by != "" && t.After(now) {
			busy = true
		}
	}
	return busy, rows.Err()
}

// Settle writes rec's reporting state and releases the claim when the stored version is still
// version.
func (s *reviewStore) Settle(ctx context.Context, rec *review.Record, version int64) (bool, error) {
	const q = `UPDATE review_reports SET phase=?, held_by=?, status_state=?, comment_sha256=?,
	recorded_sha256=?, reported_at=?, done=?, attempts=?, failing_since=?, retry_at=?, last_error=?,
	provider=?, api_url=?, repository=?, status_context=?,
	claimed_by='', claimed_until='', version=version+1
WHERE id=? AND version=?`
	rec.Sanitize()
	res, err := s.db.ExecContext(ctx, q,
		rec.Phase, rec.HeldBy, rec.StatusState, rec.CommentSHA256, rec.RecordedSHA256,
		optionalTime(rec.ReportedAt), sqlutil.BoolToInt(rec.Done), rec.Attempts,
		optionalTime(rec.FailingSince), optionalTime(rec.RetryAt), rec.LastError, rec.Provider,
		rec.APIURL, rec.Repository, rec.StatusContext, rec.ID, version)
	if err != nil {
		return false, fmt.Errorf("settle review report: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("settle review report: %w", err)
	}
	return n > 0, nil
}

// optionalTime stores a zero time as the empty string and any other in the canonical form.
func optionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return sqlutil.FormatTime(t)
}

// scanReviewRecord reads one review report row.
func scanReviewRecord(sc scanner) (*review.Record, error) {
	var (
		rec                                          review.Record
		reported, claimedUntil, failing, retry, made string
		done                                         int
	)
	if err := sc.Scan(&rec.ID, &rec.Kind, &rec.RunID, &rec.TriggerID, &rec.PullRequest,
		&rec.CommitSHA, &rec.Reason, &rec.Receipt, &rec.Phase, &rec.HeldBy, &rec.StatusState,
		&rec.CommentSHA256, &rec.RecordedSHA256, &reported, &done, &rec.Version, &rec.ClaimedBy,
		&claimedUntil, &rec.Attempts, &failing, &retry, &rec.LastError, &made, &rec.Provider,
		&rec.APIURL, &rec.Repository, &rec.StatusContext); err != nil {
		return nil, err
	}
	rec.Done = done != 0
	rec.ReportedAt = sqlutil.ParseTimeOrAbsent(reported)
	rec.ClaimedUntil = sqlutil.ParseTimeOrAbsent(claimedUntil)
	rec.FailingSince = sqlutil.ParseTimeOrAbsent(failing)
	rec.RetryAt = sqlutil.ParseTimeOrAbsent(retry)
	at, err := sqlutil.ParseTime(made)
	if err != nil {
		return nil, fmt.Errorf("decode review report created_at: %w", err)
	}
	rec.CreatedAt = at
	return &rec, nil
}
