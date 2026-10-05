package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// notificationColumns is the shared select list for notification target reads.
const notificationColumns = `id, name, description, org_id, kind, recipient, url_hint, key_set,
	needs_secret, sealed_url, sealed_key, created_at, created_by`

// attachmentColumns is the shared select list for notification attachment reads.
const attachmentColumns = `id, notification_id, object_kind, object_id, event, created_at,
	created_by`

// notificationStore is a notification.Store backed by the shared PostgreSQL database.
type notificationStore struct {
	// db is the open database handle shared with the run store.
	db *sql.DB
}

// Save inserts or replaces the target.
func (s *notificationStore) Save(ctx context.Context, n *notification.Notification) error {
	const q = `
INSERT INTO notification_targets (id, name, description, org_id, kind, recipient, url_hint, key_set,
	needs_secret, sealed_url, sealed_key, created_at, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT(id) DO UPDATE SET
	name=excluded.name, description=excluded.description, org_id=excluded.org_id,
	kind=excluded.kind, recipient=excluded.recipient, url_hint=excluded.url_hint,
	key_set=excluded.key_set, needs_secret=excluded.needs_secret, sealed_url=excluded.sealed_url,
	sealed_key=excluded.sealed_key, created_at=excluded.created_at, created_by=excluded.created_by`
	_, err := s.db.ExecContext(ctx, q, n.ID, n.Name, n.Description, n.OrgID, n.Kind, n.To,
		n.URLHint, sqlutil.BoolToInt(n.KeySet), sqlutil.BoolToInt(n.NeedsSecret), n.SealedURL,
		n.SealedKey, sqlutil.FormatTime(n.CreatedAt), n.CreatedBy)
	if err != nil {
		return fmt.Errorf("save notification target: %w", err)
	}
	return nil
}

// Update replaces an existing target, or returns notification.ErrNotFound when it is gone.
func (s *notificationStore) Update(ctx context.Context, n *notification.Notification) error {
	const q = `
UPDATE notification_targets SET name=$1, description=$2, org_id=$3, kind=$4, recipient=$5,
	url_hint=$6, key_set=$7, needs_secret=$8, sealed_url=$9, sealed_key=$10
WHERE id=$11`
	res, err := s.db.ExecContext(ctx, q, n.Name, n.Description, n.OrgID, n.Kind, n.To, n.URLHint,
		sqlutil.BoolToInt(n.KeySet), sqlutil.BoolToInt(n.NeedsSecret), n.SealedURL, n.SealedKey,
		n.ID)
	if err != nil {
		return fmt.Errorf("update notification target: %w", err)
	}
	if affected, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("update notification target: %w", err)
	} else if affected == 0 {
		return notification.ErrNotFound
	}
	return nil
}

// Get returns the target with the given id, or notification.ErrNotFound.
func (s *notificationStore) Get(ctx context.Context, id string) (*notification.Notification, error) {
	const q = "SELECT " + notificationColumns + " FROM notification_targets WHERE id=$1"
	n, err := scanNotification(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notification.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get notification target: %w", err)
	}
	return n, nil
}

// List returns every target ordered by creation time, oldest first.
func (s *notificationStore) List(ctx context.Context) ([]*notification.Notification, error) {
	const q = "SELECT " + notificationColumns + " FROM notification_targets ORDER BY created_at, id"
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list notification targets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*notification.Notification{}
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, fmt.Errorf("list notification targets: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list notification targets: %w", err)
	}
	return out, nil
}

// Delete removes the target and its attachments in one transaction, or returns
// notification.ErrNotFound.
func (s *notificationStore) Delete(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete notification target: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM notification_attachments WHERE notification_id=$1", id); err != nil {
		return fmt.Errorf("delete notification attachments: %w", err)
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM notification_targets WHERE id=$1", id)
	if err != nil {
		return fmt.Errorf("delete notification target: %w", err)
	}
	if affected, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("delete notification target: %w", err)
	} else if affected == 0 {
		return notification.ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete notification target: %w", err)
	}
	return nil
}

// Attach stores an attachment, or returns notification.ErrDuplicate when the unique index already
// holds the same target, object, and event.
func (s *notificationStore) Attach(ctx context.Context, a *notification.Attachment) error {
	const q = `
INSERT INTO notification_attachments (id, notification_id, object_kind, object_id, event,
	created_at, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)`
	_, err := s.db.ExecContext(ctx, q, a.ID, a.NotificationID, a.ObjectKind, a.ObjectID, a.Event,
		sqlutil.FormatTime(a.CreatedAt), a.CreatedBy)
	if isKeyConflict(err) {
		return notification.ErrDuplicate
	}
	if err != nil {
		return fmt.Errorf("attach notification target: %w", err)
	}
	return nil
}

// Detach removes the attachment, or returns notification.ErrAttachmentNotFound.
func (s *notificationStore) Detach(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM notification_attachments WHERE id=$1", id)
	if err != nil {
		return fmt.Errorf("detach notification target: %w", err)
	}
	if affected, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("detach notification target: %w", err)
	} else if affected == 0 {
		return notification.ErrAttachmentNotFound
	}
	return nil
}

// Attachments returns a target's attachments, oldest first.
func (s *notificationStore) Attachments(ctx context.Context,
	notificationID string) ([]*notification.Attachment, error) {
	const q = "SELECT " + attachmentColumns + ` FROM notification_attachments
WHERE notification_id=$1 ORDER BY created_at, id`
	return s.attachments(ctx, q, notificationID)
}

// AttachedTo returns the attachments on one object, oldest first.
func (s *notificationStore) AttachedTo(ctx context.Context, kind,
	objectID string) ([]*notification.Attachment, error) {
	const q = "SELECT " + attachmentColumns + ` FROM notification_attachments
WHERE object_kind=$1 AND object_id=$2 ORDER BY created_at, id`
	return s.attachments(ctx, q, kind, objectID)
}

// attachments runs one attachment query and scans its rows.
func (s *notificationStore) attachments(ctx context.Context, q string,
	args ...any) ([]*notification.Attachment, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list notification attachments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*notification.Attachment{}
	for rows.Next() {
		var (
			a       notification.Attachment
			created string
		)
		if err := rows.Scan(&a.ID, &a.NotificationID, &a.ObjectKind, &a.ObjectID, &a.Event,
			&created, &a.CreatedBy); err != nil {
			return nil, fmt.Errorf("list notification attachments: %w", err)
		}
		a.CreatedAt = sqlutil.ParseTimeOrAbsent(created)
		out = append(out, &a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list notification attachments: %w", err)
	}
	return out, nil
}

// scanNotification reads one target row from a scanner.
func scanNotification(sc scanner) (*notification.Notification, error) {
	var (
		n             notification.Notification
		keySet, needs int
		created       string
	)
	if err := sc.Scan(&n.ID, &n.Name, &n.Description, &n.OrgID, &n.Kind, &n.To, &n.URLHint,
		&keySet, &needs, &n.SealedURL, &n.SealedKey, &created, &n.CreatedBy); err != nil {
		return nil, err
	}
	n.KeySet, n.NeedsSecret = keySet != 0, needs != 0
	n.CreatedAt = sqlutil.ParseTimeOrAbsent(created)
	return &n, nil
}
