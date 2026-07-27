package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kordloom/switchtender/internal/sqlutil"
	"github.com/kordloom/switchtender/internal/team"
)

// teamStore is a team.Store backed by the shared PostgreSQL database.
type teamStore struct {
	// db is the open database handle shared with the run store.
	db *sql.DB
}

// Save inserts or replaces the team.
func (s *teamStore) Save(ctx context.Context, t *team.Team) error {
	const q = `
INSERT INTO teams (id, name, created_at)
VALUES ($1, $2, $3)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, created_at=excluded.created_at`
	if _, err := s.db.ExecContext(ctx, q, t.ID, t.Name, sqlutil.FormatTime(t.CreatedAt)); err != nil {
		return fmt.Errorf("save team: %w", err)
	}
	return nil
}

// Get returns the team with the given id, or team.ErrNotFound.
func (s *teamStore) Get(ctx context.Context, id string) (*team.Team, error) {
	const q = "SELECT id, name, created_at FROM teams WHERE id=$1"
	t, err := scanTeam(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, team.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get team: %w", err)
	}
	return t, nil
}

// List returns all teams ordered by creation time, oldest first.
func (s *teamStore) List(ctx context.Context) ([]*team.Team, error) {
	const q = "SELECT id, name, created_at FROM teams ORDER BY created_at, id"
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*team.Team
	for rows.Next() {
		t, err := scanTeam(rows)
		if err != nil {
			return nil, fmt.Errorf("list teams: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}
	return out, nil
}

// Delete removes the team and its memberships in one transaction, or returns team.ErrNotFound. The
// transaction keeps the two deletes atomic so an interrupt cannot orphan membership rows.
func (s *teamStore) Delete(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete team: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "DELETE FROM team_members WHERE team_id=$1", id); err != nil {
		return fmt.Errorf("delete team members: %w", err)
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM teams WHERE id=$1", id)
	if err != nil {
		return fmt.Errorf("delete team: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete team: %w", err)
	}
	if n == 0 {
		return team.ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete team: %w", err)
	}
	return nil
}

// AddMember adds a user to a team. Adding an existing member is a no-op.
func (s *teamStore) AddMember(ctx context.Context, teamID, userID string) error {
	const q = `INSERT INTO team_members (team_id, user_id) VALUES ($1, $2)
ON CONFLICT(team_id, user_id) DO NOTHING`
	if _, err := s.db.ExecContext(ctx, q, teamID, userID); err != nil {
		return fmt.Errorf("add team member: %w", err)
	}
	return nil
}

// RemoveMember removes a user from a team. Removing a non-member is a no-op.
func (s *teamStore) RemoveMember(ctx context.Context, teamID, userID string) error {
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM team_members WHERE team_id=$1 AND user_id=$2", teamID, userID)
	if err != nil {
		return fmt.Errorf("remove team member: %w", err)
	}
	return nil
}

// Members returns the user ids in a team, sorted.
func (s *teamStore) Members(ctx context.Context, teamID string) ([]string, error) {
	return s.memberQuery(ctx,
		"SELECT user_id FROM team_members WHERE team_id=$1 ORDER BY user_id", teamID)
}

// TeamsForUser returns the ids of the teams a user belongs to, sorted.
func (s *teamStore) TeamsForUser(ctx context.Context, userID string) ([]string, error) {
	return s.memberQuery(ctx,
		"SELECT team_id FROM team_members WHERE user_id=$1 ORDER BY team_id", userID)
}

// memberQuery runs a single-column membership query and collects the results.
func (s *teamStore) memberQuery(ctx context.Context, q, arg string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, q, arg)
	if err != nil {
		return nil, fmt.Errorf("query membership: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("query membership: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query membership: %w", err)
	}
	return out, nil
}

// scanTeam reads one team row from a scanner.
func scanTeam(sc scanner) (*team.Team, error) {
	var (
		t       team.Team
		created string
	)
	if err := sc.Scan(&t.ID, &t.Name, &created); err != nil {
		return nil, err
	}
	at, err := sqlutil.ParseTime(created)
	if err != nil {
		return nil, err
	}
	t.CreatedAt = at
	return &t, nil
}
