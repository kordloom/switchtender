package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dcadolph/yardmaster/internal/template"
)

// templateColumns is the shared select list for template reads.
const templateColumns = `id, name, project_id, playbook, inventory, shards, credential_ids,
	extra_vars, survey, queue, created_at`

// templateStore is a template.Store backed by the shared SQLite database.
type templateStore struct {
	// db is the open database handle shared with the run store.
	db *sql.DB
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
	const q = `
INSERT INTO templates
	(id, name, project_id, playbook, inventory, shards, credential_ids, extra_vars, survey, queue, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	name=excluded.name, project_id=excluded.project_id, playbook=excluded.playbook,
	inventory=excluded.inventory, shards=excluded.shards,
	credential_ids=excluded.credential_ids, extra_vars=excluded.extra_vars,
	survey=excluded.survey, queue=excluded.queue, created_at=excluded.created_at`
	_, err = s.db.ExecContext(ctx, q,
		t.ID, t.Name, t.ProjectID, t.Playbook, t.Inventory, t.Shards,
		joinIDs(t.CredentialIDs), string(vars), string(survey), t.Queue, formatTime(t.CreatedAt))
	if err != nil {
		return fmt.Errorf("save template: %w", err)
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
		t       template.Template
		creds   string
		vars    string
		survey  string
		created string
	)
	if err := sc.Scan(&t.ID, &t.Name, &t.ProjectID, &t.Playbook, &t.Inventory, &t.Shards,
		&creds, &vars, &survey, &t.Queue, &created); err != nil {
		return nil, err
	}
	t.CredentialIDs = splitIDs(creds)
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
	at, err := parseTime(created)
	if err != nil {
		return nil, err
	}
	t.CreatedAt = at
	return &t, nil
}
