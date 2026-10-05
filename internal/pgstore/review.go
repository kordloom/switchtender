package pgstore

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

// reviewStore is a review.Store backed by the shared PostgreSQL database.
type reviewStore struct {
	// db is the open database handle shared with the run store.
	db *sql.DB
}

// Create inserts rec unless a record with its id exists.
func (s *reviewStore) Create(ctx context.Context, rec *review.Record) (bool, error) {
	const q = `INSERT INTO review_reports (` + reviewColumns + `)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20,
	$21, $22, $23, $24, $25, $26, $27)
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

// PruneComments removes the command marks and done replies made before before. The stored times
// compare as text, which orders them correctly to the second, and the cutoff is days old.
func (s *reviewStore) PruneComments(ctx context.Context, before time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `
DELETE FROM review_reports
WHERE (kind='command' OR (kind='reply' AND done<>0)) AND created_at<>'' AND created_at<$1`,
		sqlutil.FormatTime(before))
	if err != nil {
		return 0, fmt.Errorf("prune comment records: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune comment records: %w", err)
	}
	return int(n), nil
}

// Get returns the record with id, or review.ErrRecordNotFound.
func (s *reviewStore) Get(ctx context.Context, id string) (*review.Record, error) {
	rec, err := scanReviewRecord(s.db.QueryRowContext(ctx,
		"SELECT "+reviewColumns+" FROM review_reports WHERE id=$1", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, review.ErrRecordNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get review report: %w", err)
	}
	return rec, nil
}

// Pending returns up to limit records not yet done, oldest first. The stored time is trimmed of its
// zone letter before it is compared, the same fix sqlutil.TimeOrder applies, and compared byte by
// byte, so neither a whole second nor the database's collation reorders it.
func (s *reviewStore) Pending(ctx context.Context, limit int) ([]*review.Record, error) {
	q := "SELECT " + reviewColumns + " FROM review_reports WHERE done=0 " +
		`ORDER BY rtrim(created_at, 'Z') COLLATE "C", id COLLATE "C"`
	args := []any{}
	if limit > 0 {
		q += " LIMIT $1"
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
// same pull request and trigger holds a claim live at now.
//
// Every record of that pull request is locked first, in id order, so two processes claiming two of
// its records at once take turns rather than each judging the lane before the other's claim
// commits, and the shared order means they can never deadlock. Under read committed a locking read
// that waited returns the row as the transaction it waited on left it, so the second claimer sees
// the first one's claim.
func (s *reviewStore) Claim(ctx context.Context, id string, version int64, owner string, now,
	until time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("claim review report: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var triggerID string
	var number int
	err = tx.QueryRowContext(ctx, "SELECT trigger_id, pull_request FROM review_reports WHERE id=$1",
		id).Scan(&triggerID, &number)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim review report: %w", err)
	}
	free, err := laneFree(ctx, tx, triggerID, number, id, version, now)
	if err != nil {
		return false, fmt.Errorf("claim review report: %w", err)
	}
	if !free {
		return false, nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE review_reports SET version=version+1, claimed_by=$1,
	claimed_until=$2 WHERE id=$3 AND version=$4 AND done=0`,
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

// laneFree locks every record of one pull request and trigger and reports whether id is claimable:
// still at version, not done, and no other record of the lane holding a claim live at now. The
// times are compared parsed rather than as text, since the stored form trims its fraction.
func laneFree(ctx context.Context, tx *sql.Tx, triggerID string, number int, id string, version int64,
	now time.Time) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, claimed_by, claimed_until, version, done
FROM review_reports WHERE trigger_id=$1 AND pull_request=$2 ORDER BY id COLLATE "C" FOR UPDATE`,
		triggerID, number)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	free, found := true, false
	for rows.Next() {
		var (
			rowID, by, until string
			rowVersion       int64
			done             int
		)
		if err := rows.Scan(&rowID, &by, &until, &rowVersion, &done); err != nil {
			return false, err
		}
		if rowID == id {
			found = true
			if rowVersion != version || done != 0 {
				free = false
			}
			continue
		}
		if by != "" && sqlutil.ParseTimeOrAbsent(until).After(now) {
			free = false
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return free && found, nil
}

// Settle writes rec's reporting state and releases the claim when the stored version is still
// version.
func (s *reviewStore) Settle(ctx context.Context, rec *review.Record, version int64) (bool, error) {
	const q = `UPDATE review_reports SET phase=$1, held_by=$2, status_state=$3, comment_sha256=$4,
	recorded_sha256=$5, reported_at=$6, done=$7, attempts=$8, failing_since=$9, retry_at=$10,
	last_error=$11, provider=$12, api_url=$13, repository=$14, status_context=$15, claimed_by='',
	claimed_until='', version=version+1
WHERE id=$16 AND version=$17`
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
