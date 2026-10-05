package pgstore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// attachable describes an object a notification target can be attached to, for deleting it.
type attachable struct {
	// table holds the object's rows, keyed by id. It is a fixed internal name, not caller input.
	table string
	// kind is the notification object kind its attachments are stored under.
	kind string
	// noun names the object in an error.
	noun string
	// notFound is the error a delete of a missing object returns.
	notFound error
	// before are statements run ahead of the object's own delete, each taking the id, such as
	// removing an organization's members.
	before []string
}

// The attachable objects, one per notification object kind.
var (
	// attachableTemplate is a job template or workflow.
	attachableTemplate = attachable{table: "templates", kind: notification.KindTemplate,
		noun: "template", notFound: template.ErrNotFound}
	// attachableSchedule is a schedule.
	attachableSchedule = attachable{table: "schedules", kind: notification.KindSchedule,
		noun: "schedule", notFound: schedule.ErrNotFound}
	// attachableProject is a project.
	attachableProject = attachable{table: "projects", kind: notification.KindProject,
		noun: "project", notFound: project.ErrNotFound}
	// attachableOrg is an organization, whose members go with it.
	attachableOrg = attachable{table: "orgs", kind: notification.KindOrg, noun: "org",
		notFound: org.ErrNotFound, before: []string{"DELETE FROM org_members WHERE org_id=$1"}}
)

// deleteAttachable removes one object and every notification attachment on it in one
// transaction, and leaves the targets themselves alone. The attachments are polymorphic, one table
// naming objects of four kinds by kind and id, so no foreign key can cascade them; this is the
// cascade, and the store contract proves it runs for every kind. With recorded set, the delete is
// refused with notification.ErrCleanupChanged, and nothing is removed, when the attachments taken
// are not the ones the delete's chain entry recorded.
func deleteAttachable(ctx context.Context, db *sql.DB, a attachable, id string,
	recorded *notification.Cleanup) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete %s: %w", a.noun, err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range a.before {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return fmt.Errorf("delete %s: %w", a.noun, err)
		}
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM "+a.table+" WHERE id=$1", id)
	if err != nil {
		return fmt.Errorf("delete %s: %w", a.noun, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("delete %s: %w", a.noun, err)
	} else if n == 0 {
		return a.notFound
	}
	rows, err := tx.QueryContext(ctx, `DELETE FROM notification_attachments
WHERE object_kind=$1 AND object_id=$2 RETURNING notification_id`, a.kind, id)
	if err != nil {
		return fmt.Errorf("delete %s notification attachments: %w", a.noun, err)
	}
	removed, err := scanIDs(rows)
	if err != nil {
		return fmt.Errorf("delete %s notification attachments: %w", a.noun, err)
	}
	if recorded != nil && !recorded.Equal(notification.Summarize(removed)) {
		return notification.ErrCleanupChanged
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete %s: %w", a.noun, err)
	}
	return nil
}

// DeleteRecorded removes the template and its notification attachments, provided they are the ones
// recorded.
func (s *templateStore) DeleteRecorded(ctx context.Context, id string,
	recorded notification.Cleanup) error {
	return deleteAttachable(ctx, s.db, attachableTemplate, id, &recorded)
}

// DeleteRecorded removes the schedule and its notification attachments, provided they are the ones
// recorded.
func (s *scheduleStore) DeleteRecorded(ctx context.Context, id string,
	recorded notification.Cleanup) error {
	return deleteAttachable(ctx, s.db, attachableSchedule, id, &recorded)
}

// DeleteRecorded removes the project and its notification attachments, provided they are the ones
// recorded.
func (s *projectStore) DeleteRecorded(ctx context.Context, id string,
	recorded notification.Cleanup) error {
	return deleteAttachable(ctx, s.db, attachableProject, id, &recorded)
}

// DeleteRecorded removes the organization, its members, and its notification attachments, provided
// the attachments are the ones recorded.
func (s *orgStore) DeleteRecorded(ctx context.Context, id string, recorded notification.Cleanup) error {
	return deleteAttachable(ctx, s.db, attachableOrg, id, &recorded)
}
