package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kordloom/switchtender/internal/sqlutil"
	"github.com/kordloom/switchtender/internal/template"
)

// templateColumns is the shared select list for template reads.
const templateColumns = `id, name, project_id, playbook, inventory, inventory_id, shards,
	credential_ids, extra_vars, survey, queue, created_at, tool, command, dry_run, image,
	pull_credential_id, org_id, notifications, selectable_credential_ids, timeout,
	confirm_on_launch, tags, skip_tags, verbosity, forks, diff_mode, steps`

// templateStore is a template.Store backed by the shared SQLite database.
type templateStore struct {
	// db is the open database handle shared with the run store.
	db *splitDB
}

// Save inserts or replaces the template.
func (s *templateStore) Save(ctx context.Context, t *template.Template) error {
	vars, err := json.Marshal(t.ExtraVars)
	if err != nil {
		return fmt.Errorf("save template: %w", err)
	}
	survey, err := json.Marshal(t.Survey)
	if err != nil {
		return fmt.Errorf("save template: %w", err)
	}
	notifs, err := json.Marshal(t.Notifications)
	if err != nil {
		return fmt.Errorf("save template: %w", err)
	}
	const q = `
INSERT INTO templates
	(id, name, project_id, playbook, inventory, inventory_id, shards, credential_ids, extra_vars,
	 survey, queue, created_at, tool, command, dry_run, image, pull_credential_id, org_id,
	 notifications, selectable_credential_ids, timeout, confirm_on_launch,
	 tags, skip_tags, verbosity, forks, diff_mode, steps)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	name=excluded.name, project_id=excluded.project_id, playbook=excluded.playbook,
	inventory=excluded.inventory, inventory_id=excluded.inventory_id, shards=excluded.shards,
	credential_ids=excluded.credential_ids, extra_vars=excluded.extra_vars,
	survey=excluded.survey, queue=excluded.queue, created_at=excluded.created_at,
	tool=excluded.tool, command=excluded.command, dry_run=excluded.dry_run,
	image=excluded.image, pull_credential_id=excluded.pull_credential_id, org_id=excluded.org_id,
	notifications=excluded.notifications,
	selectable_credential_ids=excluded.selectable_credential_ids, timeout=excluded.timeout,
	confirm_on_launch=excluded.confirm_on_launch, tags=excluded.tags,
	skip_tags=excluded.skip_tags, verbosity=excluded.verbosity, forks=excluded.forks,
	diff_mode=excluded.diff_mode, steps=excluded.steps`
	_, err = s.db.ExecContext(ctx, q,
		t.ID, t.Name, t.ProjectID, t.Playbook, t.Inventory, t.InventoryID, t.Shards,
		sqlutil.JoinIDs(t.CredentialIDs), string(vars), string(survey), t.Queue, sqlutil.FormatTime(t.CreatedAt),
		t.Tool, t.Command, sqlutil.BoolToInt(t.DryRun), t.Image, t.PullCredentialID, t.OrgID, string(notifs),
		sqlutil.JoinIDs(t.SelectableCredentialIDs), t.Timeout, sqlutil.BoolToInt(t.ConfirmOnLaunch),
		sqlutil.JoinIDs(t.Tags), sqlutil.JoinIDs(t.SkipTags), t.Verbosity, t.Forks,
		sqlutil.BoolToInt(t.DiffMode), marshalSteps(t.Steps))
	if err != nil {
		return fmt.Errorf("save template: %w", err)
	}
	return nil
}

// Update changes an existing template's fields, or returns template.ErrNotFound.
func (s *templateStore) Update(ctx context.Context, t *template.Template) error {
	vars, err := json.Marshal(t.ExtraVars)
	if err != nil {
		return fmt.Errorf("update template: %w", err)
	}
	survey, err := json.Marshal(t.Survey)
	if err != nil {
		return fmt.Errorf("update template: %w", err)
	}
	notifs, err := json.Marshal(t.Notifications)
	if err != nil {
		return fmt.Errorf("update template: %w", err)
	}
	const q = `UPDATE templates SET
	name=?, project_id=?, playbook=?, inventory=?, inventory_id=?, shards=?,
	credential_ids=?, extra_vars=?, survey=?, queue=?, tool=?, command=?, dry_run=?, image=?,
	pull_credential_id=?, org_id=?, notifications=?, selectable_credential_ids=?, timeout=?,
	confirm_on_launch=?, tags=?, skip_tags=?, verbosity=?, forks=?, diff_mode=?, steps=?
	WHERE id=?`
	res, err := s.db.ExecContext(ctx, q,
		t.Name, t.ProjectID, t.Playbook, t.Inventory, t.InventoryID, t.Shards,
		sqlutil.JoinIDs(t.CredentialIDs), string(vars), string(survey), t.Queue, t.Tool, t.Command,
		sqlutil.BoolToInt(t.DryRun), t.Image, t.PullCredentialID, t.OrgID, string(notifs),
		sqlutil.JoinIDs(t.SelectableCredentialIDs), t.Timeout, sqlutil.BoolToInt(t.ConfirmOnLaunch),
		sqlutil.JoinIDs(t.Tags), sqlutil.JoinIDs(t.SkipTags), t.Verbosity, t.Forks,
		sqlutil.BoolToInt(t.DiffMode), marshalSteps(t.Steps), t.ID)
	if err != nil {
		return fmt.Errorf("update template: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update template: %w", err)
	}
	if n == 0 {
		return template.ErrNotFound
	}
	return nil
}

// Get returns the template with the given id, or template.ErrNotFound.
func (s *templateStore) Get(ctx context.Context, id string) (*template.Template, error) {
	const q = "SELECT " + templateColumns + " FROM templates WHERE id=?"
	t, err := scanTemplate(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, template.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get template: %w", err)
	}
	return t, nil
}

// List returns all templates ordered by creation time, oldest first.
func (s *templateStore) List(ctx context.Context) ([]*template.Template, error) {
	const q = "SELECT " + templateColumns + " FROM templates ORDER BY created_at, id"
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*template.Template
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, fmt.Errorf("list templates: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	return out, nil
}

// Delete removes the template with the given id, or returns template.ErrNotFound.
func (s *templateStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM templates WHERE id=?", id)
	if err != nil {
		return fmt.Errorf("delete template: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete template: %w", err)
	}
	if n == 0 {
		return template.ErrNotFound
	}
	return nil
}

// scanTemplate reads one template row from a scanner.
func scanTemplate(sc scanner) (*template.Template, error) {
	var (
		t          template.Template
		creds      string
		vars       string
		survey     string
		created    string
		dryRun     int
		notifs     string
		selectable string
		confirm    int
		tags       string
		skipTags   string
		diffMode   int
		steps      string
	)
	if err := sc.Scan(&t.ID, &t.Name, &t.ProjectID, &t.Playbook, &t.Inventory, &t.InventoryID,
		&t.Shards, &creds, &vars, &survey, &t.Queue, &created, &t.Tool, &t.Command,
		&dryRun, &t.Image, &t.PullCredentialID, &t.OrgID, &notifs, &selectable, &t.Timeout,
		&confirm, &tags, &skipTags, &t.Verbosity, &t.Forks, &diffMode, &steps); err != nil {
		return nil, err
	}
	t.DryRun = dryRun != 0
	t.ConfirmOnLaunch = confirm != 0
	t.DiffMode = diffMode != 0
	t.Tags = sqlutil.SplitIDs(tags)
	t.SkipTags = sqlutil.SplitIDs(skipTags)
	t.CredentialIDs = sqlutil.SplitIDs(creds)
	t.SelectableCredentialIDs = sqlutil.SplitIDs(selectable)
	if vars != "" && vars != "null" {
		if err := json.Unmarshal([]byte(vars), &t.ExtraVars); err != nil {
			return nil, err
		}
	}
	if survey != "" && survey != "null" {
		if err := json.Unmarshal([]byte(survey), &t.Survey); err != nil {
			return nil, err
		}
	}
	if notifs != "" && notifs != "null" {
		if err := json.Unmarshal([]byte(notifs), &t.Notifications); err != nil {
			return nil, err
		}
	}
	at, err := sqlutil.ParseTime(created)
	if err != nil {
		return nil, err
	}
	if steps != "" {
		if t.Steps, err = parseSteps(steps); err != nil {
			return nil, err
		}
	}
	t.CreatedAt = at
	return &t, nil
}
