package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dcadolph/yardmaster/internal/audit"
)

// auditStore is an audit.Store backed by the shared SQLite database.
type auditStore struct {
	// db is the open database handle shared with the run store.
	db *sql.DB
}

// Append records one entry.
func (s *auditStore) Append(ctx context.Context, e *audit.Entry) error {
	const q = "INSERT INTO audit_entries (id, at, actor, method, path) VALUES (?, ?, ?, ?, ?)"
	if _, err := s.db.ExecContext(ctx, q,
		e.ID, formatTime(e.At), e.Actor, e.Method, e.Path); err != nil {
		return fmt.Errorf("append audit entry: %w", err)
	}
	return nil
}

// List returns up to limit entries, newest first.
func (s *auditStore) List(ctx context.Context, limit int) ([]*audit.Entry, error) {
	if limit < 1 {
		limit = 1
	}
	const q = `SELECT id, at, actor, method, path FROM audit_entries
ORDER BY at DESC, id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("list audit entries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*audit.Entry
	for rows.Next() {
		var (
			e  audit.Entry
			at string
		)
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Method, &e.Path); err != nil {
			return nil, fmt.Errorf("list audit entries: %w", err)
		}
		if e.At, err = parseTime(at); err != nil {
			return nil, fmt.Errorf("list audit entries: %w", err)
		}
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list audit entries: %w", err)
	}
	return out, nil
}
