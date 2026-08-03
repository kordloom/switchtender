package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// rowQuerier runs a single-row query. Both *sql.DB and *sql.Tx satisfy it, so the chain head can be
// read inside the append transaction.
type rowQuerier interface {
	// QueryRowContext runs the query and returns at most one row.
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// auditStore is an audit.Store backed by the shared SQLite database.
type auditStore struct {
	// db is the open database handle shared with the run store.
	db *splitDB
	// mu serializes appends within this process so the hash chain reads its head and inserts
	// atomically. A UNIQUE index on seq is the cross-process backstop. It is not what limits append
	// throughput: the write pool is a single connection, so a transaction already holds every other
	// writer off for its whole life, and dropping the mutex measures the same either way.
	mu sync.Mutex
}

// Append records one entry, linking it to the current chain head inside a transaction so the head
// read and the insert are atomic. The unique seq index rejects a fork from a second process. A
// span marker entry is refused: only AppendSpanBeat mints beats.
func (s *auditStore) Append(ctx context.Context, e *audit.Entry) error {
	if audit.IsSpanMarker(e) {
		return fmt.Errorf("append audit entry: %w", audit.ErrReservedSpan)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("append audit entry: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	prev, err := s.head(ctx, tx)
	if err != nil {
		return err
	}
	cp := *e
	audit.Link(prev, &cp)
	const q = `INSERT INTO audit_entries (id, at, actor, method, path, seq, prev_hash, hash)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := tx.ExecContext(ctx, q,
		cp.ID, sqlutil.FormatTime(cp.At), cp.Actor, cp.Method, cp.Path, cp.Seq, cp.PrevHash, cp.Hash); err != nil {
		return fmt.Errorf("append audit entry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("append audit entry: %w", err)
	}
	*e = cp
	return nil
}

// AppendSpanBeat mints and appends the next span beat under the append mutex and inside a
// transaction, so the beat read, the count, and the insert are one atomic step and concurrent
// callers cannot mint the same beat.
func (s *auditStore) AppendSpanBeat(ctx context.Context, at time.Time, cadenceS int) (*audit.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("append span beat: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	prev, err := s.head(ctx, tx)
	if err != nil {
		return nil, err
	}
	var headSeq int64
	if prev != nil {
		headSeq = prev.Seq
	}
	lastSpanSeq, lastSpanBeat, err := lastSpan(ctx, tx)
	if err != nil {
		return nil, err
	}
	beat, count := audit.NextSpanBeat(headSeq, lastSpanSeq, lastSpanBeat)
	e := audit.NewSpanEntry(at, beat, count, cadenceS)
	audit.Link(prev, e)
	const q = `INSERT INTO audit_entries (id, at, actor, method, path, seq, prev_hash, hash)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := tx.ExecContext(ctx, q,
		e.ID, sqlutil.FormatTime(e.At), e.Actor, e.Method, e.Path, e.Seq, e.PrevHash, e.Hash); err != nil {
		return nil, fmt.Errorf("append span beat: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("append span beat: %w", err)
	}
	return e, nil
}

// lastSpan returns the newest well-formed span entry's sequence and beat, or zeros when the chain
// holds none. Span-marked rows are walked newest first, and one whose path does not round-trip is
// skipped: it is an ordinary entry that merely wears the actor, so it must not supply a beat.
func lastSpan(ctx context.Context, tx *sql.Tx) (int64, int64, error) {
	const q = `SELECT seq, path FROM audit_entries WHERE actor = ? AND method = ?
ORDER BY seq DESC`
	rows, err := tx.QueryContext(ctx, q, audit.SpanActor, audit.SpanMethod)
	if err != nil {
		return 0, 0, fmt.Errorf("last span: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			seq  int64
			path string
		)
		if err := rows.Scan(&seq, &path); err != nil {
			return 0, 0, fmt.Errorf("last span: %w", err)
		}
		if beat, _, _, ok := audit.ParseSpanPath(path); ok {
			return seq, beat, nil
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("last span: %w", err)
	}
	return 0, 0, nil
}

// head returns the current chain head, the entry with the highest sequence, or nil when empty. It
// reads through the given querier so the caller can scope it to the append transaction.
func (s *auditStore) head(ctx context.Context, q rowQuerier) (*audit.Entry, error) {
	const query = "SELECT seq, hash FROM audit_entries ORDER BY seq DESC, id DESC LIMIT 1"
	var e audit.Entry
	switch err := q.QueryRowContext(ctx, query).Scan(&e.Seq, &e.Hash); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("audit head: %w", err)
	}
	return &e, nil
}

// SpanBeats returns the newest limit span beat entries, oldest first. The database narrows to
// span-marked rows and hands them back newest first, and the scan stops once limit well-formed
// beats are in hand, so the feed never loads the whole chain. A near-miss row that wears the
// marker without a round-tripping path is an ordinary entry and is skipped without using a slot.
func (s *auditStore) SpanBeats(ctx context.Context, limit int) ([]*audit.Entry, error) {
	if limit < 1 {
		limit = 1
	}
	const q = `SELECT id, at, actor, method, path, seq, prev_hash, hash FROM audit_entries
WHERE actor = ? AND method = ? ORDER BY seq DESC, id DESC`
	rows, err := s.db.QueryContext(ctx, q, audit.SpanActor, audit.SpanMethod)
	if err != nil {
		return nil, fmt.Errorf("span beats: %w", err)
	}
	return scanSpanBeats(rows, limit)
}

// List returns up to limit entries, newest first.
func (s *auditStore) List(ctx context.Context, limit int) ([]*audit.Entry, error) {
	if limit < 1 {
		limit = 1
	}
	const q = `SELECT id, at, actor, method, path, seq, prev_hash, hash FROM audit_entries
ORDER BY seq DESC, id DESC LIMIT ?`
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

// scanSpanBeats reads span-marked rows handed back newest first, keeps the first limit well-formed
// beats, and returns them oldest first, closing rows.
func scanSpanBeats(rows *sql.Rows, limit int) ([]*audit.Entry, error) {
	defer func() { _ = rows.Close() }()
	out := []*audit.Entry{}
	for len(out) < limit && rows.Next() {
		var (
			e  audit.Entry
			at string
		)
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Method, &e.Path,
			&e.Seq, &e.PrevHash, &e.Hash); err != nil {
			return nil, fmt.Errorf("scan span beat: %w", err)
		}
		var err error
		if e.At, err = sqlutil.ParseTime(at); err != nil {
			return nil, fmt.Errorf("scan span beat: %w", err)
		}
		if !audit.IsSpanMarker(&e) {
			continue
		}
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan span beats: %w", err)
	}
	slices.Reverse(out)
	return out, nil
}

// scanAudit reads audit rows into entries, closing rows.
func scanAudit(rows *sql.Rows) ([]*audit.Entry, error) {
	defer func() { _ = rows.Close() }()
	out := []*audit.Entry{}
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
		if e.At, err = sqlutil.ParseTime(at); err != nil {
			return nil, fmt.Errorf("scan audit entry: %w", err)
		}
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan audit entries: %w", err)
	}
	return out, nil
}

// SaveAnchor records one anchor, which fixes a chain link somewhere this install cannot rewrite
// alone.
func (s *auditStore) SaveAnchor(ctx context.Context, a *audit.Anchor) error {
	const q = `INSERT INTO audit_anchors (id, type, seq, link, at, ref, proof)
VALUES (?, ?, ?, ?, ?, ?, ?)`
	if _, err := s.db.ExecContext(ctx, q,
		a.ID, a.Type, a.Seq, a.Link, sqlutil.FormatTime(a.At), a.Ref, a.Proof); err != nil {
		return fmt.Errorf("save anchor: %w", err)
	}
	return nil
}

// Anchors returns every anchor at or below seq, oldest first. A seq of zero or less returns all of
// them, since a caller with no range in mind wants the whole set.
func (s *auditStore) Anchors(ctx context.Context, seq int64) ([]*audit.Anchor, error) {
	q := "SELECT id, type, seq, link, at, ref, proof FROM audit_anchors"
	args := []any{}
	if seq > 0 {
		q += " WHERE seq <= ?"
		args = append(args, seq)
	}
	q += " ORDER BY seq ASC, id ASC"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list anchors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*audit.Anchor{}
	for rows.Next() {
		var a audit.Anchor
		var at string
		if err := rows.Scan(&a.ID, &a.Type, &a.Seq, &a.Link, &at, &a.Ref, &a.Proof); err != nil {
			return nil, fmt.Errorf("list anchors: %w", err)
		}
		parsed, err := sqlutil.ParseTime(at)
		if err != nil {
			return nil, fmt.Errorf("list anchors: %w", err)
		}
		a.At = parsed
		out = append(out, &a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list anchors: %w", err)
	}
	return out, nil
}
