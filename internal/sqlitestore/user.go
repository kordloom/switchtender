package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kordloom/switchtender/internal/sqlutil"
	"github.com/kordloom/switchtender/internal/user"
)

// userColumns is the shared select list for user reads.
const userColumns = `id, username, password_hash, role, created_at, full_name, email, phone,
	title, links, notes, source`

// userStore is a user.Store backed by the shared SQLite database.
type userStore struct {
	// db is the open database handle shared with the run store.
	db *splitDB
}

// Save inserts or replaces the user.
func (s *userStore) Save(ctx context.Context, u *user.User) error {
	links, err := marshalLinks(u.Links)
	if err != nil {
		return fmt.Errorf("save user: %w", err)
	}
	const q = `
INSERT INTO users (id, username, password_hash, role, created_at,
	full_name, email, phone, title, links, notes, source)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	username=excluded.username, password_hash=excluded.password_hash,
	role=excluded.role, created_at=excluded.created_at, full_name=excluded.full_name,
	email=excluded.email, phone=excluded.phone, title=excluded.title, links=excluded.links,
	notes=excluded.notes, source=excluded.source`
	if _, err := s.db.ExecContext(ctx, q,
		u.ID, u.Username, u.PasswordHash, string(u.Role), sqlutil.FormatTime(u.CreatedAt),
		u.FullName, u.Email, u.Phone, u.Title, links, u.Notes, u.Source); err != nil {
		return fmt.Errorf("save user: %w", err)
	}
	return nil
}

// Update changes an existing user's username, role, password hash, and profile, or returns
// user.ErrNotFound.
func (s *userStore) Update(ctx context.Context, u *user.User) error {
	links, err := marshalLinks(u.Links)
	if err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		"UPDATE users SET username=?, password_hash=?, role=?, full_name=?, email=?, phone=?, "+
			"title=?, links=?, notes=? WHERE id=?",
		u.Username, u.PasswordHash, string(u.Role), u.FullName, u.Email, u.Phone, u.Title, links,
		u.Notes, u.ID)
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

// DeleteUnlessLastAdmin removes the user unless doing so would leave no administrator.
//
// The guard is part of the statement rather than a count taken beforehand. Counting first left a gap
// another request could pass the same count in: two concurrent deletes of the last two admins both
// saw a survivor and both went through, leaving an install nobody can administer and no way back in
// except a shell on the host.
func (s *userStore) DeleteUnlessLastAdmin(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM users WHERE id=? AND (role<>'admin'
OR EXISTS (SELECT 1 FROM users others WHERE others.role='admin' AND others.id<>?))`, id, id)
	if err != nil {
		return false, fmt.Errorf("delete user: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete user: %w", err)
	}
	if n > 0 {
		return true, nil
	}
	// Nothing changed for one of two reasons, and they are different answers to the caller.
	if _, gerr := s.Get(ctx, id); gerr != nil {
		return false, gerr
	}
	return false, nil
}

// UpdateUnlessLastAdmin applies the update unless it would demote the only administrator. Demoting
// is the other way to reach zero admins, and it is guarded in the statement for the same reason.
func (s *userStore) UpdateUnlessLastAdmin(ctx context.Context, u *user.User) (bool, error) {
	links, err := marshalLinks(u.Links)
	if err != nil {
		return false, fmt.Errorf("update user: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET username=?, password_hash=?, role=?, full_name=?, email=?, phone=?,
title=?, links=?, notes=? WHERE id=? AND (?='admin' OR role<>'admin'
OR EXISTS (SELECT 1 FROM users others WHERE others.role='admin' AND others.id<>?))`,
		u.Username, u.PasswordHash, string(u.Role), u.FullName, u.Email, u.Phone, u.Title, links,
		u.Notes, u.ID, string(u.Role), u.ID)
	if err != nil {
		return false, fmt.Errorf("update user: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("update user: %w", err)
	}
	if n > 0 {
		return true, nil
	}
	if _, gerr := s.Get(ctx, u.ID); gerr != nil {
		return false, gerr
	}
	return false, nil
}

// scanUser reads one user row from a scanner.
func scanUser(sc scanner) (*user.User, error) {
	var (
		u       user.User
		role    string
		created string
		links   string
	)
	if err := sc.Scan(&u.ID, &u.Username, &u.PasswordHash, &role, &created,
		&u.FullName, &u.Email, &u.Phone, &u.Title, &links, &u.Notes, &u.Source); err != nil {
		return nil, err
	}
	u.Role = user.Role(role)
	at, err := sqlutil.ParseTime(created)
	if err != nil {
		return nil, err
	}
	u.CreatedAt = at
	u.Links, err = unmarshalLinks(links)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// marshalLinks encodes a user's profile links for storage. Links are held as a JSON array rather than
// a joined string because a URL may legally contain any separator a join would pick.
func marshalLinks(links []string) (string, error) {
	if len(links) == 0 {
		return "", nil
	}
	out, err := json.Marshal(links)
	if err != nil {
		return "", fmt.Errorf("encode links: %w", err)
	}
	return string(out), nil
}

// unmarshalLinks decodes stored profile links, treating an empty column as none.
func unmarshalLinks(stored string) ([]string, error) {
	if stored == "" {
		return nil, nil
	}
	var links []string
	if err := json.Unmarshal([]byte(stored), &links); err != nil {
		return nil, fmt.Errorf("decode links: %w", err)
	}
	return links, nil
}
