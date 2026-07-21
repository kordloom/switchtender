package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kordloom/switchtender/internal/user"
)

// userColumns is the shared select list for user reads.
const userColumns = `id, username, password_hash, role, created_at`

// userStore is a user.Store backed by the shared SQLite database.
type userStore struct {
	// db is the open database handle shared with the run store.
	db *sql.DB
}

// Save inserts or replaces the user.
func (s *userStore) Save(ctx context.Context, u *user.User) error {
	const q = `
INSERT INTO users (id, username, password_hash, role, created_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	username=excluded.username, password_hash=excluded.password_hash,
	role=excluded.role, created_at=excluded.created_at`
	_, err := s.db.ExecContext(ctx, q,
		u.ID, u.Username, u.PasswordHash, string(u.Role), formatTime(u.CreatedAt))
	if err != nil {
		return fmt.Errorf("save user: %w", err)
	}
	return nil
}

// Update changes an existing user's username, role, and password hash, or returns user.ErrNotFound.
func (s *userStore) Update(ctx context.Context, u *user.User) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE users SET username=?, password_hash=?, role=? WHERE id=?",
		u.Username, u.PasswordHash, string(u.Role), u.ID)
	if err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	if n == 0 {
		return user.ErrNotFound
	}
	return nil
}

// Get returns the user with the given id, or user.ErrNotFound.
func (s *userStore) Get(ctx context.Context, id string) (*user.User, error) {
	const q = "SELECT " + userColumns + " FROM users WHERE id=?"
	u, err := scanUser(s.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, user.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

// FindByUsername returns the user with the given username, or user.ErrNotFound.
func (s *userStore) FindByUsername(ctx context.Context, username string) (*user.User, error) {
	const q = "SELECT " + userColumns + " FROM users WHERE username=?"
	u, err := scanUser(s.db.QueryRowContext(ctx, q, username))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, user.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find user: %w", err)
	}
	return u, nil
}

// List returns all users ordered by creation time, oldest first.
func (s *userStore) List(ctx context.Context) ([]*user.User, error) {
	const q = "SELECT " + userColumns + " FROM users ORDER BY created_at, id"
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*user.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("list users: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return out, nil
}

// Delete removes the user with the given id, or returns user.ErrNotFound.
func (s *userStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM users WHERE id=?", id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if n == 0 {
		return user.ErrNotFound
	}
	return nil
}

// scanUser reads one user row from a scanner.
func scanUser(sc scanner) (*user.User, error) {
	var (
		u       user.User
		role    string
		created string
	)
	if err := sc.Scan(&u.ID, &u.Username, &u.PasswordHash, &role, &created); err != nil {
		return nil, err
	}
	u.Role = user.Role(role)
	at, err := parseTime(created)
	if err != nil {
		return nil, err
	}
	u.CreatedAt = at
	return &u, nil
}
