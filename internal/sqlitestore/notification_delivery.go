package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/notification"
)

// deliveryColumns is the shared select list for notification delivery reads, prefixed d.
const deliveryColumns = `d.notification_id, d.run_id, d.seq, d.event, d.branch, d.follows,
	d.target_name, d.target_kind, d.status, d.attempts, d.next_attempt_ms, d.claimed_by,
	d.claim_until_ms, d.last_error, d.note, d.created_ms, d.finished_ms`

// ListAttachments returns every attachment, oldest first.
func (s *notificationStore) ListAttachments(ctx context.Context) ([]*notification.Attachment, error) {
	const q = "SELECT " + attachmentColumns +
		" FROM notification_attachments ORDER BY created_at, id"
	return s.attachments(ctx, q)
}

// DetachObject removes every attachment on one object and summarizes what it removed.
func (s *notificationStore) DetachObject(ctx context.Context, kind,
	objectID string) (notification.Cleanup, error) {
	rows, err := s.db.w.QueryContext(ctx, `DELETE FROM notification_attachments
WHERE object_kind=? AND object_id=? RETURNING notification_id`, kind, objectID)
	if err != nil {
		return notification.Cleanup{}, fmt.Errorf("detach notification attachments: %w", err)
	}
	targets, err := scanIDs(rows)
	if err != nil {
		return notification.Cleanup{}, fmt.Errorf("detach notification attachments: %w", err)
	}
	return notification.Summarize(targets), nil
}

// Record appends the event to its run's sequence and queues one delivery to each recipient in one
// transaction. The transaction takes the write lock when it begins, so two events of one run can
// never take the same sequence number, and a run's end, start, or hold announced twice is recorded
// once.
func (s *notificationStore) Record(ctx context.Context, ev *notification.RunEvent,
	recipients []notification.Recipient) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("record notification event: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	key := ev.DedupeKey()
	first, second := ev.Once()
	var have int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM notification_events
WHERE run_id=? AND (dedupe_key=? OR (branch=? AND event IN (?, ?)))`, ev.RunID, key, ev.Branch,
		first, second).Scan(&have); err != nil {
		return false, fmt.Errorf("record notification event: %w", err)
	}
	if have > 0 {
		return false, nil
	}
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM notification_events
WHERE run_id=?`, ev.RunID).Scan(&seq); err != nil {
		return false, fmt.Errorf("record notification event: %w", err)
	}
	created, follows := ev.CreatedAt.UnixMilli(), notification.EncodeFollows(ev.Follows)
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_events
	(run_id, seq, event, branch, dedupe_key, snapshot, created_ms) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ev.RunID, seq, ev.Event, ev.Branch, key, string(ev.Snapshot), created); err != nil {
		return false, fmt.Errorf("record notification event: %w", err)
	}
	for _, rc := range recipients {
		status, lastError, finished := notification.DeliveryPending, "", int64(0)
		if rc.Skip != "" {
			status, lastError, finished = notification.DeliverySkipped, rc.Skip, created
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_deliveries
	(notification_id, run_id, seq, event, branch, follows, target_name, target_kind, status,
	 next_attempt_ms, last_error, created_ms, finished_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (notification_id, run_id, seq) DO NOTHING`,
			rc.NotificationID, ev.RunID, seq, ev.Event, ev.Branch, follows, rc.Name, rc.Kind,
			status, created, lastError, created, finished); err != nil {
			return false, fmt.Errorf("queue notification delivery: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("record notification event: %w", err)
	}
	ev.Seq = seq
	return true, nil
}

// claimQuery selects the deliveries a claim may take: pending, due, under no unexpired claim, and
// with no earlier delivery to the same target for the same run still pending, unless that one is on
// a different branch of a workflow. It carries each one's snapshot and whether an earlier delivery
// to the same target for the same run failed. The %s takes the clause that leaves out the targets
// whose share the claimant already holds.
const claimQuery = `SELECT ` + deliveryColumns + `, e.snapshot,
	EXISTS (SELECT 1 FROM notification_deliveries f WHERE f.notification_id = d.notification_id
		AND f.run_id = d.run_id AND f.seq < d.seq AND f.status = 'failed')
FROM notification_deliveries d
JOIN notification_events e ON e.run_id = d.run_id AND e.seq = d.seq
WHERE d.status = 'pending' AND d.next_attempt_ms <= ? AND d.claim_until_ms < ?
	AND NOT EXISTS (SELECT 1 FROM notification_deliveries b
		WHERE b.notification_id = d.notification_id AND b.run_id = d.run_id AND b.seq < d.seq
		AND b.status = 'pending' AND (d.branch = '' OR b.branch = '' OR b.branch = d.branch
			OR instr(d.follows, char(10) || b.branch || char(10)) > 0))%s
ORDER BY d.next_attempt_ms, d.run_id, d.seq, d.notification_id
LIMIT ?`

// Claim leases up to limit due deliveries to owner, at most perTarget to one target counting the
// ones owner already holds when perTarget is above zero. The read and the leases are one
// transaction holding the write lock, so two claimants never take the same delivery.
func (s *notificationStore) Claim(ctx context.Context, owner string, now time.Time,
	lease time.Duration, limit, perTarget int) ([]*notification.Delivery, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("claim notification deliveries: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	at := now.UnixMilli()
	held, err := heldByOwner(ctx, tx, owner, at, perTarget)
	if err != nil {
		return nil, fmt.Errorf("claim notification deliveries: %w", err)
	}
	args := []any{at, at}
	var full []string
	for target, n := range held {
		if n >= perTarget {
			full = append(full, target)
			args = append(args, target)
		}
	}
	skip := ""
	if len(full) > 0 {
		skip = "\n\tAND d.notification_id NOT IN (" +
			strings.TrimSuffix(strings.Repeat("?, ", len(full)), ", ") + ")"
	}
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(claimQuery, skip), append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("claim notification deliveries: %w", err)
	}
	candidates, err := scanClaimed(rows)
	if err != nil {
		return nil, fmt.Errorf("claim notification deliveries: %w", err)
	}
	until := now.Add(lease)
	out := make([]*notification.Delivery, 0, len(candidates))
	for _, d := range candidates {
		if perTarget > 0 && held[d.NotificationID] >= perTarget {
			continue
		}
		res, err := tx.ExecContext(ctx, `UPDATE notification_deliveries
SET claimed_by=?, claim_until_ms=?
WHERE notification_id=? AND run_id=? AND seq=? AND status='pending' AND claim_until_ms < ?`,
			owner, until.UnixMilli(), d.NotificationID, d.RunID, d.Seq, at)
		if err != nil {
			return nil, fmt.Errorf("claim notification delivery: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return nil, fmt.Errorf("claim notification delivery: %w", err)
		} else if n == 1 {
			d.ClaimedBy, d.ClaimUntil = owner, msTime(until.UnixMilli())
			held[d.NotificationID]++
			out = append(out, d)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("claim notification deliveries: %w", err)
	}
	return out, nil
}

// heldByOwner counts, by target, the deliveries owner holds under unexpired claims at the instant
// at, or nothing when no share is kept.
func heldByOwner(ctx context.Context, tx *sql.Tx, owner string, at int64,
	perTarget int) (map[string]int, error) {
	held := map[string]int{}
	if perTarget <= 0 {
		return held, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT notification_id, COUNT(*) FROM notification_deliveries
WHERE status = 'pending' AND claimed_by = ? AND claim_until_ms >= ? GROUP BY notification_id`,
		owner, at)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var target string
		var n int
		if err := rows.Scan(&target, &n); err != nil {
			return nil, err
		}
		held[target] = n
	}
	return held, rows.Err()
}

// Finish records what an attempt at a claimed delivery came to and releases the claim, or returns
// notification.ErrDeliveryLost when owner no longer holds it.
func (s *notificationStore) Finish(ctx context.Context, d *notification.Delivery, owner string,
	out notification.Outcome) error {
	out.SanitizeText()
	status, next, finished := out.Status, int64(0), int64(0)
	switch status {
	case notification.DeliveryDelivered, notification.DeliveryFailed,
		notification.DeliverySkipped:
		finished = out.At.UnixMilli()
		next = d.NextAttemptAt.UnixMilli()
	default:
		status, next = notification.DeliveryPending, out.NextAttemptAt.UnixMilli()
	}
	res, err := s.db.ExecContext(ctx, `UPDATE notification_deliveries
SET status=?, attempts=attempts+1, last_error=?, note=?, next_attempt_ms=?, finished_ms=?,
	claimed_by='', claim_until_ms=0
WHERE notification_id=? AND run_id=? AND seq=? AND status='pending' AND claimed_by=?`,
		status, out.Error, out.Note, next, finished, d.NotificationID, d.RunID, d.Seq, owner)
	if err != nil {
		return fmt.Errorf("finish notification delivery: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("finish notification delivery: %w", err)
	} else if n == 0 {
		return notification.ErrDeliveryLost
	}
	return nil
}

// Release lets go of owner's claim on a delivery without counting an attempt, leaving it due as it
// was, or returns notification.ErrDeliveryLost when owner no longer holds it.
func (s *notificationStore) Release(ctx context.Context, d *notification.Delivery,
	owner string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE notification_deliveries
SET claimed_by='', claim_until_ms=0
WHERE notification_id=? AND run_id=? AND seq=? AND status='pending' AND claimed_by=?`,
		d.NotificationID, d.RunID, d.Seq, owner)
	if err != nil {
		return fmt.Errorf("release notification delivery: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("release notification delivery: %w", err)
	} else if n == 0 {
		return notification.ErrDeliveryLost
	}
	return nil
}

// Deliveries lists the deliveries a filter selects, newest first.
func (s *notificationStore) Deliveries(ctx context.Context,
	f notification.DeliveryFilter) ([]*notification.Delivery, error) {
	var where []string
	var args []any
	if f.RunID != "" {
		where, args = append(where, "d.run_id=?"), append(args, f.RunID)
	}
	if f.NotificationID != "" {
		where, args = append(where, "d.notification_id=?"), append(args, f.NotificationID)
	}
	if f.Status != "" {
		where, args = append(where, "d.status=?"), append(args, f.Status)
	}
	if !f.Since.IsZero() {
		where, args = append(where, "d.created_ms>=?"), append(args, f.Since.UnixMilli())
	}
	q := "SELECT " + deliveryColumns + " FROM notification_deliveries d"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY d.created_ms DESC, d.run_id DESC, d.seq DESC, d.notification_id LIMIT ?"
	args = append(args, f.EffectiveLimit())
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list notification deliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*notification.Delivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, fmt.Errorf("list notification deliveries: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list notification deliveries: %w", err)
	}
	return out, nil
}

// scanClaimed reads claim candidates, each with its snapshot and whether an earlier delivery
// failed, and closes the rows.
func scanClaimed(rows *sql.Rows) ([]*notification.Delivery, error) {
	defer func() { _ = rows.Close() }()
	var out []*notification.Delivery
	for rows.Next() {
		var (
			snapshot string
			failed   bool
		)
		d, err := scanDelivery(rows, &snapshot, &failed)
		if err != nil {
			return nil, err
		}
		d.Snapshot, d.EarlierFailed = []byte(snapshot), failed
		out = append(out, d)
	}
	return out, rows.Err()
}

// scanDelivery reads one delivery row, scanning any extra columns after it into extra.
func scanDelivery(sc scanner, extra ...any) (*notification.Delivery, error) {
	var (
		d                              notification.Delivery
		follows                        string
		next, until, created, finished int64
	)
	dest := []any{&d.NotificationID, &d.RunID, &d.Seq, &d.Event, &d.Branch, &follows,
		&d.TargetName, &d.TargetKind, &d.Status, &d.Attempts, &next, &d.ClaimedBy, &until,
		&d.LastError, &d.Note, &created, &finished}
	if err := sc.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	d.Follows = notification.DecodeFollows(follows)
	d.NextAttemptAt, d.CreatedAt = *msTime(next), *msTime(created)
	if until > 0 {
		d.ClaimUntil = msTime(until)
	}
	if finished > 0 {
		d.FinishedAt = msTime(finished)
	}
	return &d, nil
}

// scanIDs reads one string column from every row and closes the rows.
func scanIDs(rows *sql.Rows) ([]string, error) {
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// msTime returns the instant a stored Unix millisecond count names, in UTC.
func msTime(ms int64) *time.Time {
	t := time.UnixMilli(ms).UTC()
	return &t
}

// purgeNotifications removes the notification events and deliveries of the purgeable runs created
// before cut, which retention is about to delete. They are the record of what was sent about a run,
// read on the run's page, so they go with it rather than outliving it as rows nothing can open.
func (s *store) purgeNotifications(ctx context.Context, cut string) error {
	for _, table := range []string{"notification_deliveries", "notification_events"} {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE run_id IN (
	SELECT id FROM runs WHERE `+purgeableRun+` AND created_at < ?)`, cut); err != nil {
			return fmt.Errorf("purge %s: %w", table, err)
		}
	}
	return nil
}
