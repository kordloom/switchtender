package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/dcadolph/yardmaster/internal/project"
)

// projectColumns is the shared select list for project reads.
const projectColumns = `id, name, repo_url, branch, credential_id, install_deps, image, pull_credential_id, created_at`

// projectStore is a project.Store backed by the shared SQLite database.
type projectStore struct {
	// db is the open database handle shared with the run store.
	db *sql.DB
}

// Save inserts or replaces the project.
func (s *projectStore) Save(ctx context.Context, p *project.Project) error {
	const q = `
INSERT INTO projects
	(id, name, repo_url, branch, credential_id, install_deps, image, pull_credential_id, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	name=excluded.name, repo_url=excluded.repo_url, branch=excluded.branch,
	credential_id=excluded.credential_id, install_deps=excluded.install_deps,
	image=excluded.image, pull_credential_id=excluded.pull_credential_id,
	created_at=excluded.created_at`
	_, err := s.db.ExecContext(ctx, q,
		p.ID, p.Name, p.RepoURL, p.Branch, p.CredentialID,
		boolToInt(p.InstallDeps), p.Image, p.PullCredentialID, formatTime(p.CreatedAt))
	if err != nil {
		return fmt.Errorf("save project: %w", err)
	}
	return nil
}

// Update changes an existing project's mutable fields, or returns project.ErrNotFound.
func (s *projectStore) Update(ctx context.Context, p *project.Project) error {
	const q = `UPDATE projects SET
	name=?, repo_url=?, branch=?, credential_id=?, install_deps=?, image=?, pull_credential_id=?
	WHERE id=?`
	res, err := s.db.ExecContext(ctx, q,
		p.Name, p.RepoURL, p.Branch, p.CredentialID,
		boolToInt(p.InstallDeps), p.Image, p.PullCredentialID, p.ID)
	if err != nil {
		return fmt.Errorf("update project: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update project: %w", err)
	}
	if n == 0 {
		return project.ErrNotFound
	}
	return nil
}

// Get returns the project with the given id, or project.ErrNotFound.
func (s *projectStore) Get(ctx context.Context, id string) (*project.Project, error) {
	const q = "SELECT " + projectColumns + " FROM projects WHERE id=?"
	p, err := scanProject(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, project.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	return p, nil
}

// List returns all projects ordered by creation time, oldest first.
func (s *projectStore) List(ctx context.Context) ([]*project.Project, error) {
	const q = "SELECT " + projectColumns + " FROM projects ORDER BY created_at, id"
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*project.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("list projects: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	return out, nil
}

// Delete removes the project with the given id, or returns project.ErrNotFound.
func (s *projectStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM projects WHERE id=?", id)
	if err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	if n == 0 {
		return project.ErrNotFound
	}
	return nil
}

// scanProject reads one project row from a scanner.
func scanProject(sc scanner) (*project.Project, error) {
	var (
		p           project.Project
		installDeps int
		created     string
	)
	if err := sc.Scan(&p.ID, &p.Name, &p.RepoURL, &p.Branch, &p.CredentialID,
		&installDeps, &p.Image, &p.PullCredentialID, &created); err != nil {
		return nil, err
	}
	p.InstallDeps = installDeps != 0
	at, err := parseTime(created)
	if err != nil {
		return nil, err
	}
	p.CreatedAt = at
	return &p, nil
}
