package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// decisionColumns is the shared select list for decision record reads.
const decisionColumns = `id, kind, decision_id, run_id, step_run_id, verdict, recorded_at, actor,
	actor_type, on_behalf_of, reason_text, reason_random, reason_commitment, reason_masked,
	redaction, sod`

// decisionStore is a decision.Store backed by the shared SQLite database.
type decisionStore struct {
	// db is the open database handle shared with the run store.
	db *splitDB
}

// Decisions returns the approval decision record store: approver reasons, corrections, and the
// separation-of-duties evaluations of agent-initiated runs.
func (d *DB) Decisions() decision.Store {
	return &decisionStore{db: d.db}
}

// Save inserts a record, refusing an id another record holds.
func (s *decisionStore) Save(ctx context.Context, r *decision.Record) error {
	const q = `
INSERT INTO run_decisions (` + decisionColumns + `)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	text, random, commitment, masked, redaction, err := reasonColumns(r)
	if err != nil {
		return err
	}
	sod, err := sodColumn(r.SeparationOfDuties)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, q, r.ID, r.Kind, r.DecisionID, r.RunID, r.StepRunID,
		r.Verdict, sqlutil.FormatTime(r.At), r.Actor, r.ActorType, r.OnBehalfOf, text, random,
		commitment, masked, redaction, sod); err != nil {
		if isKeyConflict(err) {
			return decision.ErrExists
		}
		return fmt.Errorf("save decision record: %w", err)
	}
	return nil
}

// Get returns the record with the given id.
func (s *decisionStore) Get(ctx context.Context, id string) (*decision.Record, error) {
	r, err := scanDecision(s.db.QueryRowContext(ctx,
		"SELECT "+decisionColumns+" FROM run_decisions WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, decision.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read decision record: %w", err)
	}
	return r, nil
}

// ForRun returns a run's records, oldest first.
func (s *decisionStore) ForRun(ctx context.Context, runID string) ([]*decision.Record, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+decisionColumns+" FROM run_decisions WHERE run_id=?", runID)
	if err != nil {
		return nil, fmt.Errorf("list decision records: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*decision.Record
	for rows.Next() {
		r, err := scanDecision(rows)
		if err != nil {
			return nil, fmt.Errorf("list decision records: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list decision records: %w", err)
	}
	// Ordered here rather than in SQL, for the reason federation keys are: the stored text sorts an
	// instant with a fraction after the whole second it falls in.
	decision.SortRecords(out)
	return out, nil
}

// Redact clears the reason text and random value in one conditional update, so two redactions
// racing cannot both succeed and a record with no reason is never marked as redacted.
func (s *decisionStore) Redact(ctx context.Context, id string, red decision.Redaction) error {
	raw, err := json.Marshal(red)
	if err != nil {
		return fmt.Errorf("encode redaction: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE run_decisions SET reason_text='', reason_random='', redaction=?
WHERE id=? AND reason_commitment<>'' AND redaction=''`, string(raw), id)
	if err != nil {
		return fmt.Errorf("redact reason: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("redact reason: %w", err)
	}
	if n == 1 {
		return nil
	}
	return redactRefusal(ctx, s, id)
}

// FinishRedaction clears the pending mark of the named redaction, compared against the stored
// value itself, so a redaction finished or replaced in between is never overwritten.
func (s *decisionStore) FinishRedaction(ctx context.Context, id, entryID string) error {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT redaction FROM run_decisions WHERE id=?", id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return decision.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("finish redaction: %w", err)
	}
	var red decision.Redaction
	if raw == "" || json.Unmarshal([]byte(raw), &red) != nil || red.EntryID != entryID {
		return decision.ErrRedacted
	}
	if !red.Pending {
		return nil
	}
	red.Pending = false
	done, err := json.Marshal(red)
	if err != nil {
		return fmt.Errorf("encode redaction: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		"UPDATE run_decisions SET redaction=? WHERE id=? AND redaction=?", string(done), id, raw)
	if err != nil {
		return fmt.Errorf("finish redaction: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("finish redaction: %w", err)
	}
	if n == 1 {
		return nil
	}
	// Changed in between, by a second finisher of the same redaction or not: read it again.
	return s.FinishRedaction(ctx, id, entryID)
}

// PendingRedactions returns the records whose redaction is pending, oldest first.
func (s *decisionStore) PendingRedactions(ctx context.Context) ([]*decision.Record, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+decisionColumns+" FROM run_decisions WHERE redaction LIKE ?", `%"pending":true%`)
	if err != nil {
		return nil, fmt.Errorf("list pending redactions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*decision.Record
	for rows.Next() {
		r, err := scanDecision(rows)
		if err != nil {
			return nil, fmt.Errorf("list pending redactions: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending redactions: %w", err)
	}
	decision.SortRecords(out)
	return out, nil
}

// Delete removes a record.
func (s *decisionStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM run_decisions WHERE id=?", id)
	if err != nil {
		return fmt.Errorf("delete decision record: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete decision record: %w", err)
	}
	if n == 0 {
		return decision.ErrNotFound
	}
	return nil
}

// redactRefusal says why a redaction changed nothing: the record is missing, has no reason, or was
// already redacted.
func redactRefusal(ctx context.Context, s decision.Store, id string) error {
	r, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if !r.HasReason() {
		return decision.ErrNoReason
	}
	return decision.ErrRedacted
}

// reasonColumns flattens a record's reason into the columns that store it.
func reasonColumns(r *decision.Record) (text, random, commitment string, masked int, redaction string,
	err error) {
	if r.Reason == nil {
		return "", "", "", 0, "", nil
	}
	if r.Reason.Redacted != nil {
		raw, merr := json.Marshal(r.Reason.Redacted)
		if merr != nil {
			return "", "", "", 0, "", fmt.Errorf("encode redaction: %w", merr)
		}
		redaction = string(raw)
	}
	return r.Reason.Text, r.Reason.Random, r.Reason.Commitment, sqlutil.BoolToInt(r.Reason.Masked),
		redaction, nil
}

// sodColumn encodes a separation-of-duties evaluation, the empty string for none.
func sodColumn(sod *decision.SeparationOfDuties) (string, error) {
	if sod == nil {
		return "", nil
	}
	raw, err := json.Marshal(sod)
	if err != nil {
		return "", fmt.Errorf("encode separation of duties: %w", err)
	}
	return string(raw), nil
}

// scanDecision reads one decision record row.
func scanDecision(sc scanner) (*decision.Record, error) {
	var (
		r                                            decision.Record
		at, text, random, commitment, redaction, sod string
		masked                                       int
	)
	if err := sc.Scan(&r.ID, &r.Kind, &r.DecisionID, &r.RunID, &r.StepRunID, &r.Verdict, &at,
		&r.Actor, &r.ActorType, &r.OnBehalfOf, &text, &random, &commitment, &masked, &redaction,
		&sod); err != nil {
		return nil, err
	}
	when, err := sqlutil.ParseTime(at)
	if err != nil {
		return nil, err
	}
	r.At = when
	if commitment != "" {
		r.Reason = &decision.Reason{Text: text, Random: random, Commitment: commitment,
			Masked: masked != 0}
		if redaction != "" {
			var red decision.Redaction
			if err := json.Unmarshal([]byte(redaction), &red); err != nil {
				return nil, fmt.Errorf("parse redaction: %w", err)
			}
			r.Reason.Redacted = &red
		}
	}
	if sod != "" {
		var eval decision.SeparationOfDuties
		if err := json.Unmarshal([]byte(sod), &eval); err != nil {
			return nil, fmt.Errorf("parse separation of duties: %w", err)
		}
		r.SeparationOfDuties = &eval
	}
	return &r, nil
}
