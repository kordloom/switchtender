package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// auditLockKey is the advisory-lock key that serializes audit appends. It is held for the life of
// the append transaction, so writers on any connection or process take turns rather than reading
// the same chain head and forking it. It differs from the migration lock key.
const auditLockKey = 7973821002

// rowQuerier runs a single-row query. Both *sql.DB and *sql.Tx satisfy it, so the chain head can be
// read inside the append transaction.
type rowQuerier interface {
	// QueryRowContext runs the query and returns at most one row.
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// auditStore is an audit.Store backed by the shared PostgreSQL database.
type auditStore struct {
	// installID stamps every appended entry, empty when no identity is bound. It is set once at
	// startup, before the server accepts a request, so it needs no lock of its own.
	installID string
	// db is the open database handle shared with the run store.
	db *sql.DB
}

// Append records one entry, linking it to the current chain head inside a transaction that holds an
// advisory lock, so concurrent writes on any process serialize and cannot fork the chain. A span
// marker entry is refused: only AppendSpanBeat mints beats. A run's second outcome entry is refused
// with audit.ErrOutcomeRecorded, checked under the same lock, so two replicas that both found an
// outcome owed cannot both commit it.
func (s *auditStore) Append(ctx context.Context, e *audit.Entry) error {
	if audit.IsSpanMarker(e) {
		return fmt.Errorf("append audit entry: %w", audit.ErrReservedSpan)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("append audit entry: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", auditLockKey); err != nil {
		return fmt.Errorf("append audit entry: %w", err)
	}
	if runID, ok := audit.OutcomeRunID(e); ok {
		held, herr := outcomeHeld(ctx, tx, runID)
		if herr != nil {
			return fmt.Errorf("append audit entry: %w", herr)
		}
		if held {
			return fmt.Errorf("append audit entry: run %s: %w", runID, audit.ErrOutcomeRecorded)
		}
	}
	prev, err := s.head(ctx, tx)
	if err != nil {
		return err
	}
	now, err := lockedClock(ctx, tx)
	if err != nil {
		return fmt.Errorf("append audit entry: %w", err)
	}
	cp := *e
	audit.BindEntryInstall(&cp, s.installID)
	// The time is stamped here, under the same lock that assigns the sequence, so the two can never
	// disagree, and from the database's clock, the one clock every replica sharing this chain reads
	// alike. A caller that set its own time keeps it, unless it is later than that clock: a replica
	// whose own clock ran fast used to date its entries ahead, and the pin then carried every later
	// entry from every replica forward with them.
	audit.StampAppendTime(prev, &cp, now)
	audit.Link(prev, &cp)
	const q = `INSERT INTO audit_entries (id, at, actor, actor_type, on_behalf_of, method, path, content_digest, seq, prev_hash, hash, nonce, install_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`
	if _, err := tx.ExecContext(ctx, q,
		cp.ID, sqlutil.FormatTime(cp.At), cp.Actor, cp.ActorType, cp.OnBehalfOf, cp.Method,
		cp.Path, cp.ContentDigest, cp.Seq, cp.PrevHash, cp.Hash, cp.Nonce, cp.InstallID); err != nil {
		if isConstraintConflict(err, "audit_entries_pkey") {
			return fmt.Errorf("append audit entry %s: %w", cp.ID, audit.ErrDuplicateID)
		}
		return fmt.Errorf("append audit entry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("append audit entry: %w", err)
	}
	*e = cp
	return nil
}

// AppendSpanBeat mints and appends the next span beat inside a transaction holding the audit
// advisory lock, following the guardedAdminChange precedent: readers do not block writers here, so
// only the lock makes two replicas take turns instead of both minting the same beat. It takes the
// append lock rather than a separate span key because the count and the link must also hold
// against a concurrent ordinary Append, and this is the lock that serializes the chain.
//
// A time that does not advance past the newest beat, or that falls behind the newest entry, is
// refused with audit.ErrClockBehind and nothing is written: a beat's time is a signed claim, so
// writing a time the clock did not read would be a false statement in an attestation. The skipped
// beat surfaces as a reported gap, and its number waits for the next beat the chain accepts. A zero
// time is read from the database's clock once the lock is held, the clock every entry is stamped
// with here.
func (s *auditStore) AppendSpanBeat(ctx context.Context, at time.Time, cadenceS int) (*audit.Entry, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("append span beat: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", auditLockKey); err != nil {
		return nil, fmt.Errorf("append span beat: %w", err)
	}
	prev, err := s.head(ctx, tx)
	if err != nil {
		return nil, err
	}
	if at.IsZero() {
		if at, err = lockedClock(ctx, tx); err != nil {
			return nil, fmt.Errorf("append span beat: %w", err)
		}
	}
	var headSeq int64
	if prev != nil {
		headSeq = prev.Seq
	}
	lastSpanSeq, lastSpanBeat, lastSpanAt, err := lastSpan(ctx, tx)
	if err != nil {
		return nil, err
	}
	beat, count := audit.NextSpanBeat(headSeq, lastSpanSeq, lastSpanBeat)
	// The time is checked inside this transaction, against the same newest beat the numbering came
	// from, so a clock that stepped backward skips the beat instead of minting one that fails every
	// bundle covering the pair. See audit.CheckBeatAdvance.
	if err := audit.CheckBeatAdvance(at, lastSpanAt, beat); err != nil {
		return nil, fmt.Errorf("append span beat: %w", err)
	}
	if prev != nil {
		if err := audit.CheckBeatAfterHead(at, prev.At, beat); err != nil {
			return nil, fmt.Errorf("append span beat: %w", err)
		}
	}
	e := audit.NewSpanEntry(at, beat, count, cadenceS)
	audit.BindEntryInstall(e, s.installID)
	audit.Link(prev, e)
	const q = `INSERT INTO audit_entries (id, at, actor, actor_type, on_behalf_of, method, path, content_digest, seq, prev_hash, hash, nonce, install_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`
	if _, err := tx.ExecContext(ctx, q,
		e.ID, sqlutil.FormatTime(e.At), e.Actor, e.ActorType, e.OnBehalfOf, e.Method, e.Path,
		e.ContentDigest, e.Seq, e.PrevHash, e.Hash, e.Nonce, e.InstallID); err != nil {
		return nil, fmt.Errorf("append span beat: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("append span beat: %w", err)
	}
	return e, nil
}

// lastSpan returns the newest well-formed span entry's sequence, beat, and recorded time, or zeros
// when the chain holds none. Span-marked rows are walked newest first, and one whose path does not
// round-trip is skipped: it is an ordinary entry that merely wears the actor, so it must not supply
// a beat. The time comes back with the rest because the next beat is checked against it, and both
// reads have to see the same row.
func lastSpan(ctx context.Context, tx *sql.Tx) (int64, int64, time.Time, error) {
	const q = `SELECT seq, path, at FROM audit_entries WHERE actor = $1 AND method = $2
ORDER BY seq DESC`
	rows, err := tx.QueryContext(ctx, q, audit.SpanActor, audit.SpanMethod)
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("last span: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			seq  int64
			path string
			at   string
		)
		if err := rows.Scan(&seq, &path, &at); err != nil {
			return 0, 0, time.Time{}, fmt.Errorf("last span: %w", err)
		}
		beat, _, _, ok := audit.ParseSpanPath(path)
		if !ok {
			continue
		}
		parsed, err := sqlutil.ParseTime(at)
		if err != nil {
			return 0, 0, time.Time{}, fmt.Errorf("last span: %w", err)
		}
		return seq, beat, parsed, nil
	}
	if err := rows.Err(); err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("last span: %w", err)
	}
	return 0, 0, time.Time{}, nil
}

// pgClockText reads the database's clock at the moment the statement runs, in the stored time form.
// It is clock_timestamp rather than now, which is when the transaction began: an append's
// transaction begins before its lock is granted, and the time has to be read once the lock is held.
// A federation key change reads it for the same reason, so a change that waited for the key lock
// judges the keys at the moment it got the lock, after every change it waited for.
const pgClockText = `to_char(clock_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')`

// lockedClock returns the database's clock, read through q, which an append scopes to its
// transaction once it holds the append lock.
func lockedClock(ctx context.Context, q rowQuerier) (time.Time, error) {
	var raw string
	if err := q.QueryRowContext(ctx, "SELECT "+pgClockText).Scan(&raw); err != nil {
		return time.Time{}, fmt.Errorf("read the database clock: %w", err)
	}
	now, err := sqlutil.ParseTime(raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("read the database clock: %w", err)
	}
	return now, nil
}

// outcomeHeld reports whether the chain already holds an outcome entry for runID. The paths are
// matched exactly, one for each terminal status, so the lookup rides idx_audit_outcome, which holds
// only outcome entries, and an exact match answers the same on any collation the database uses.
func outcomeHeld(ctx context.Context, q rowQuerier, runID string) (bool, error) {
	paths := outcomePaths(runID)
	marks := make([]string, len(paths))
	for i := range paths {
		marks[i] = "$" + strconv.Itoa(i+1)
	}
	query := "SELECT EXISTS (SELECT 1 FROM audit_entries WHERE method = '" + audit.MethodRun +
		"' AND path IN (" + strings.Join(marks, ", ") + "))"
	var held bool
	if err := q.QueryRowContext(ctx, query, paths...).Scan(&held); err != nil {
		return false, fmt.Errorf("find run %s outcome: %w", runID, err)
	}
	return held, nil
}

// outcomePaths returns every path an outcome entry for runID can carry, one for each terminal
// status, as query arguments.
func outcomePaths(runID string) []any {
	var paths []any
	for _, st := range run.AllStatuses() {
		if st.Terminal() {
			paths = append(paths, audit.OutcomePath(runID, string(st)))
		}
	}
	return paths
}

// head returns the current chain head, the entry with the highest sequence, or nil when empty. It
// reads through the given querier so the caller can scope it to the append transaction.
func (s *auditStore) head(ctx context.Context, q rowQuerier) (*audit.Entry, error) {
	const query = "SELECT seq, hash, at FROM audit_entries ORDER BY seq DESC LIMIT 1"
	var e audit.Entry
	var at string
	switch err := q.QueryRowContext(ctx, query).Scan(&e.Seq, &e.Hash, &at); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("audit head: %w", err)
	}
	// The head's time rides along so the append can pin the next entry against it: a chain whose
	// recorded times invert reads as edited, and with two servers on one chain the writer's own
	// clock is not enough to prevent that.
	if parsed, perr := sqlutil.ParseTime(at); perr == nil {
		e.At = parsed
	}
	return &e, nil
}

// SpanBeats returns the newest limit span beat entries, oldest first. The database narrows to
// span-marked rows and hands them back newest first, and the scan stops once limit well-formed
// beats are in hand, so the feed never loads the whole chain. A near-miss row that wears the
// marker without a round-tripping path is an ordinary entry and is skipped without using a slot, so
// the SQL limit is a multiple of what was asked for rather than the limit itself. See
// audit.SpanScanLimit.
func (s *auditStore) SpanBeats(ctx context.Context, limit int) ([]*audit.Entry, error) {
	if limit < 1 {
		limit = 1
	}
	const q = `SELECT id, at, actor, actor_type, on_behalf_of, method, path, content_digest, seq, prev_hash, hash, nonce, install_id FROM audit_entries
WHERE actor = $1 AND method = $2 ORDER BY seq DESC LIMIT $3`
	rows, err := s.db.QueryContext(ctx, q, audit.SpanActor, audit.SpanMethod, audit.SpanScanLimit(limit))
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
	const q = `SELECT id, at, actor, actor_type, on_behalf_of, method, path, content_digest, seq, prev_hash, hash, nonce, install_id FROM audit_entries
ORDER BY seq DESC LIMIT $1`
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("list audit entries: %w", err)
	}
	return scanAudit(rows)
}

// Chain returns every entry in chain order, oldest first, for verification.
func (s *auditStore) Chain(ctx context.Context) ([]*audit.Entry, error) {
	const q = `SELECT id, at, actor, actor_type, on_behalf_of, method, path, content_digest, seq, prev_hash, hash, nonce, install_id FROM audit_entries
ORDER BY seq ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("chain audit entries: %w", err)
	}
	return scanAudit(rows)
}

// ChainScan streams every entry in chain order, oldest first, one row at a time, so verifying a
// long trail never materializes it.
func (s *auditStore) ChainScan(ctx context.Context, afterSeq int64, fn func(*audit.Entry) error) error {
	const q = `SELECT id, at, actor, actor_type, on_behalf_of, method, path, content_digest, seq, prev_hash, hash, nonce, install_id FROM audit_entries
WHERE seq > $1 ORDER BY seq ASC`
	rows, err := s.db.QueryContext(ctx, q, afterSeq)
	if err != nil {
		return fmt.Errorf("chain scan audit entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			e  audit.Entry
			at string
		)
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.ActorType, &e.OnBehalfOf, &e.Method, &e.Path,
			&e.ContentDigest, &e.Seq, &e.PrevHash, &e.Hash, &e.Nonce, &e.InstallID); err != nil {
			return fmt.Errorf("chain scan audit entries: %w", err)
		}
		if e.At, err = sqlutil.ParseTime(at); err != nil {
			return fmt.Errorf("chain scan audit entries: %w", err)
		}
		if err := fn(&e); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("chain scan audit entries: %w", err)
	}
	return nil
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
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.ActorType, &e.OnBehalfOf, &e.Method, &e.Path,
			&e.ContentDigest, &e.Seq, &e.PrevHash, &e.Hash, &e.Nonce, &e.InstallID); err != nil {
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
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.ActorType, &e.OnBehalfOf, &e.Method, &e.Path,
			&e.ContentDigest, &e.Seq, &e.PrevHash, &e.Hash, &e.Nonce, &e.InstallID); err != nil {
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
	const q = `INSERT INTO audit_anchors (id, type, shape, seq, link, at, ref, proof, install_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	if _, err := s.db.ExecContext(ctx, q,
		a.ID, a.Type, a.Shape, a.Seq, a.Link, sqlutil.FormatTime(a.At), a.Ref, a.Proof,
		a.InstallID); err != nil {
		return fmt.Errorf("save anchor: %w", err)
	}
	return nil
}

// DeleteAnchor removes the anchor with the given id, or reports audit.ErrAnchorNotFound.
func (s *auditStore) DeleteAnchor(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM audit_anchors WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("delete anchor: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete anchor: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("delete anchor %s: %w", id, audit.ErrAnchorNotFound)
	}
	return nil
}

// Anchors returns every anchor at or below seq, oldest first. A seq of zero or less returns all of
// them, since a caller with no range in mind wants the whole set.
func (s *auditStore) Anchors(ctx context.Context, seq int64) ([]*audit.Anchor, error) {
	q := "SELECT id, type, shape, seq, link, at, ref, proof, install_id FROM audit_anchors"
	args := []any{}
	if seq > 0 {
		q += " WHERE seq <= $1"
		args = append(args, seq)
	}
	q += " ORDER BY seq ASC"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list anchors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []*audit.Anchor{}
	for rows.Next() {
		var a audit.Anchor
		var at string
		if err := rows.Scan(&a.ID, &a.Type, &a.Shape, &a.Seq, &a.Link, &at, &a.Ref, &a.Proof,
			&a.InstallID); err != nil {
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

// BindInstall sets the install every later append is stamped with.
func (s *auditStore) BindInstall(installID string) { s.installID = installID }

// BoundInstall returns the install id of the newest entry that carries one. It walks the sequence
// index backward and stops at the first bound entry, which on a running install is the newest one.
func (s *auditStore) BoundInstall(ctx context.Context) (string, error) {
	const q = `SELECT install_id FROM audit_entries WHERE install_id <> '' ORDER BY seq DESC LIMIT 1`
	var id string
	err := s.db.QueryRowContext(ctx, q).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the install the chain is bound to: %w", err)
	}
	return id, nil
}
