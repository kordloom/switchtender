package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/kordloom/switchtender/internal/sqlutil"
	"github.com/kordloom/switchtender/internal/template"
)

// awxBindingColumns is the shared select list for AWX callback binding reads.
const awxBindingColumns = `awx_id, template_id, awx_org, awx_name, created_at, updated_at,
	last_called_at`

// BindAWX records that an AWX job template id reaches the template b names. The insert and the
// same-object update are one statement, so two imports racing for one id cannot both win: the
// update applies only while the stored organization and name still match, and a row it leaves
// untouched is the conflict.
func (s *templateStore) BindAWX(ctx context.Context, b template.AWXBinding) error {
	const q = `
INSERT INTO awx_callback_bindings
	(awx_id, template_id, awx_org, awx_name, created_at, updated_at, last_called_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT(awx_id) DO UPDATE SET template_id=excluded.template_id, updated_at=excluded.updated_at
WHERE awx_callback_bindings.awx_org=excluded.awx_org
	AND awx_callback_bindings.awx_name=excluded.awx_name`
	res, err := s.db.ExecContext(ctx, q, b.AWXID, b.TemplateID, b.Organization, b.Name,
		sqlutil.FormatTime(b.CreatedAt), sqlutil.FormatTime(b.UpdatedAt),
		sqlutil.NullTime(b.LastCalledAt))
	if err != nil {
		return fmt.Errorf("bind awx job template %d: %w", b.AWXID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("bind awx job template %d: %w", b.AWXID, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: awx job template %d", template.ErrAWXConflict, b.AWXID)
	}
	return nil
}

// UnbindAWX removes the binding of b's id while it still names b's AWX object and template, in one
// conditional statement, so a binding another import has since pointed elsewhere is left alone.
func (s *templateStore) UnbindAWX(ctx context.Context, b template.AWXBinding) error {
	const q = `DELETE FROM awx_callback_bindings
WHERE awx_id=$1 AND awx_org=$2 AND awx_name=$3 AND template_id=$4`
	if _, err := s.db.ExecContext(ctx, q, b.AWXID, b.Organization, b.Name, b.TemplateID); err != nil {
		return fmt.Errorf("unbind awx job template %d: %w", b.AWXID, err)
	}
	return nil
}

// AWXBindingFor returns the binding of an AWX job template id, or template.ErrNotFound.
func (s *templateStore) AWXBindingFor(ctx context.Context, awxID int64) (*template.AWXBinding, error) {
	const q = "SELECT " + awxBindingColumns + " FROM awx_callback_bindings WHERE awx_id=$1"
	b, err := scanAWXBinding(s.db.QueryRowContext(ctx, q, awxID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, template.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get awx binding: %w", err)
	}
	return b, nil
}

// AWXBindings returns every binding ordered by AWX id.
func (s *templateStore) AWXBindings(ctx context.Context) ([]template.AWXBinding, error) {
	const q = "SELECT " + awxBindingColumns + " FROM awx_callback_bindings ORDER BY awx_id"
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list awx bindings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []template.AWXBinding
	for rows.Next() {
		b, err := scanAWXBinding(rows)
		if err != nil {
			return nil, fmt.Errorf("list awx bindings: %w", err)
		}
		out = append(out, *b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list awx bindings: %w", err)
	}
	return out, nil
}

// MarkAWXCalled records when a host last called through the AWX-compatible address of awxID.
func (s *templateStore) MarkAWXCalled(ctx context.Context, awxID int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE awx_callback_bindings SET last_called_at=$1 WHERE awx_id=$2",
		sqlutil.FormatTime(at), awxID)
	if err != nil {
		return fmt.Errorf("mark awx callback: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark awx callback: %w", err)
	}
	if n == 0 {
		return template.ErrNotFound
	}
	return nil
}

// scanAWXBinding reads one binding row from a scanner.
func scanAWXBinding(sc scanner) (*template.AWXBinding, error) {
	var (
		b       template.AWXBinding
		created string
		updated string
		called  sql.NullString
	)
	if err := sc.Scan(&b.AWXID, &b.TemplateID, &b.Organization, &b.Name, &created, &updated,
		&called); err != nil {
		return nil, err
	}
	var err error
	if b.CreatedAt, err = sqlutil.ParseTime(created); err != nil {
		return nil, err
	}
	if b.UpdatedAt, err = sqlutil.ParseTime(updated); err != nil {
		return nil, err
	}
	if called.Valid && called.String != "" {
		at, err := sqlutil.ParseTime(called.String)
		if err != nil {
			return nil, err
		}
		b.LastCalledAt = &at
	}
	return &b, nil
}
