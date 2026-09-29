package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kordloom/switchtender/internal/org"
)

// orgStore is an org.Store backed by the shared PostgreSQL database.
type orgStore struct {
	// db is the open database handle shared with the run store.
	db *sql.DB
}

// Save inserts or replaces the organization.
func (s *orgStore) Save(ctx context.Context, o *org.Org) error {
	const q = `
INSERT INTO orgs (id, name, created_at)
VALUES ($1, $2, $3)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, created_at=excluded.created_at`
	if _, err := s.db.ExecContext(ctx, q, o.ID, o.Name, formatTime(o.CreatedAt)); err != nil {
		return fmt.Errorf("save org: %w", err)
	}
	return nil
}

// Get returns the organization with the given id, or org.ErrNotFound.
func (s *orgStore) Get(ctx context.Context, id string) (*org.Org, error) {
	const q = "SELECT id, name, created_at FROM orgs WHERE id=$1"
	o, err := scanOrg(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, org.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get org: %w", err)
	}
	return o, nil
}

// List returns all organizations ordered by creation time, oldest first.
func (s *orgStore) List(ctx context.Context) ([]*org.Org, error) {
	const q = "SELECT id, name, created_at FROM orgs ORDER BY created_at, id"
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list orgs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*org.Org
	for rows.Next() {
		o, err := scanOrg(rows)
		if err != nil {
			return nil, fmt.Errorf("list orgs: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list orgs: %w", err)
	}
	return out, nil
}

// Delete removes the organization and its memberships in one transaction, or returns org.ErrNotFound.
// The transaction keeps the two deletes atomic so an interrupt cannot orphan membership rows.
func (s *orgStore) Delete(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete org: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "DELETE FROM org_members WHERE org_id=$1", id); err != nil {
		return fmt.Errorf("delete org members: %w", err)
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM orgs WHERE id=$1", id)
	if err != nil {
		return fmt.Errorf("delete org: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete org: %w", err)
	}
	if n == 0 {
		return org.ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete org: %w", err)
	}
	return nil
}

// AddMember adds a user to an organization with a role, or updates an existing member's role.
func (s *orgStore) AddMember(ctx context.Context, orgID, userID string, role org.Role) error {
	const q = `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, $3)
ON CONFLICT(org_id, user_id) DO UPDATE SET role=excluded.role`
	if _, err := s.db.ExecContext(ctx, q, orgID, userID, string(role)); err != nil {
		return fmt.Errorf("add org member: %w", err)
	}
	return nil
}

// RemoveMember removes a user from an organization. Removing a non-member is a no-op.
func (s *orgStore) RemoveMember(ctx context.Context, orgID, userID string) error {
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM org_members WHERE org_id=$1 AND user_id=$2", orgID, userID)
	if err != nil {
		return fmt.Errorf("remove org member: %w", err)
	}
	return nil
}

// Members returns an organization's members sorted by user id.
func (s *orgStore) Members(ctx context.Context, orgID string) ([]org.Member, error) {
	const q = "SELECT user_id, role FROM org_members WHERE org_id=$1 ORDER BY user_id"
	rows, err := s.db.QueryContext(ctx, q, orgID)
	if err != nil {
		return nil, fmt.Errorf("org members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []org.Member
	for rows.Next() {
		var (
			m    org.Member
			role string
		)
		if err := rows.Scan(&m.UserID, &role); err != nil {
			return nil, fmt.Errorf("org members: %w", err)
		}
		m.Role = org.Role(role)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("org members: %w", err)
	}
	return out, nil
}

// OrgsForUser returns the organizations a user belongs to, sorted by org id.
func (s *orgStore) OrgsForUser(ctx context.Context, userID string) ([]org.Membership, error) {
	const q = "SELECT org_id, role FROM org_members WHERE user_id=$1 ORDER BY org_id"
	rows, err := s.db.QueryContext(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("orgs for user: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []org.Membership
	for rows.Next() {
		var (
			m    org.Membership
			role string
		)
		if err := rows.Scan(&m.OrgID, &role); err != nil {
			return nil, fmt.Errorf("orgs for user: %w", err)
		}
		m.Role = org.Role(role)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("orgs for user: %w", err)
	}
	return out, nil
}

// scanOrg reads one org row from a scanner.
func scanOrg(sc scanner) (*org.Org, error) {
	var (
		o       org.Org
		created string
	)
	if err := sc.Scan(&o.ID, &o.Name, &created); err != nil {
		return nil, err
	}
	at, err := parseTime(created)
	if err != nil {
		return nil, err
	}
	o.CreatedAt = at
	return &o, nil
}
