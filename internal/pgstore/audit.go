package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/dcadolph/yardmaster/internal/audit"
)

// auditStore is an audit.Store backed by the shared PostgreSQL database.
type auditStore struct {
	// db is the open database handle shared with the run store.
	db *sql.DB
	// mu serializes appends so the hash chain reads its head and inserts atomically.
	mu sync.Mutex
}

// Append records one entry, linking it to the current chain head under a lock so concurrent writes
// cannot fork the chain.
func (s *auditStore) Append(ctx context.Context, e *audit.Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, err := s.head(ctx)
	if err != nil {
		return err
	}
	cp := *e
	audit.Link(prev, &cp)
	const q = `INSERT INTO audit_entries (id, at, actor, method, path, seq, prev_hash, hash)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	if _, err := s.db.ExecContext(ctx, q,
		cp.ID, formatTime(cp.At), cp.Actor, cp.Method, cp.Path, cp.Seq, cp.PrevHash, cp.Hash); err != nil {
		return fmt.Errorf("append audit entry: %w", err)
	}
	*e = cp
	return nil
}

// head returns the current chain head, the entry with the highest sequence, or nil when empty.
func (s *auditStore) head(ctx context.Context) (*audit.Entry, error) {
	const q = "SELECT seq, hash FROM audit_entries ORDER BY seq DESC, id DESC LIMIT 1"
	var e audit.Entry
	switch err := s.db.QueryRowContext(ctx, q).Scan(&e.Seq, &e.Hash); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("audit head: %w", err)
	}
	return &e, nil
}

// List returns up to limit entries, newest first.
func (s *auditStore) List(ctx context.Context, limit int) ([]*audit.Entry, error) {
	if limit < 1 {
		limit = 1
	}
	const q = `SELECT id, at, actor, method, path, seq, prev_hash, hash FROM audit_entries
ORDER BY seq DESC, id DESC LIMIT $1`
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("list audit entries: %w", err)
	}
	return scanAudit(rows)
}

// Chain returns every entry in chain order, oldest first, for verification.
func (s *auditStore) Chain(ctx context.Context) ([]*audit.Entry, error) {
	const q = `SELECT id, at, actor, method, path, seq, prev_hash, hash FROM audit_entries
ORDER BY seq ASC, id ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("chain audit entries: %w", err)
	}
	return scanAudit(rows)
}

// scanAudit reads audit rows into entries, closing rows.
func scanAudit(rows *sql.Rows) ([]*audit.Entry, error) {
	defer func() { _ = rows.Close() }()
	var out []*audit.Entry
	for rows.Next() {
		var (
			e  audit.Entry
			at string
		)
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Method, &e.Path,
			&e.Seq, &e.PrevHash, &e.Hash); err != nil {
			return nil, fmt.Errorf("scan audit entry: %w", err)
		}
		var err error
		if e.At, err = parseTime(at); err != nil {
			return nil, fmt.Errorf("scan audit entry: %w", err)
		}
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan audit entries: %w", err)
	}
	return out, nil
}
