package sqlitestore

import (
	"context"
	"fmt"
	"time"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// heldStatuses is the predicate for a run that reads as pending_approval: one waiting for a
// decision, a workflow parked at an approval step, and one a decision has claimed.
const heldStatuses = "status IN ('pending_approval', 'parked', 'deciding')"

// ClaimDecision records the decision that will settle a held run in one conditional update. The
// row is stored as deciding until the decision settles it, so every write that moves a run out of
// pending_approval, here and in an earlier release, passes over it.
func (s *store) ClaimDecision(ctx context.Context, id, decisionID, claim string) (bool, error) {
	if decisionID == "" || claim == "" {
		return false, run.ErrNoDecision
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE runs SET status=?, decision_id=?, decision_claim=?
WHERE id=? AND status=? AND claimed_by='' AND cancel_requested=0`,
		run.StoredDeciding, decisionID, claim, id, string(run.StatusPendingApproval))
	if err != nil {
		return false, fmt.Errorf("claim decision: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim decision: %w", err)
	}
	return n > 0, nil
}

// SettleDecision moves a run the named decision claimed to where the decision sends it and clears
// the claim, in one conditional update.
func (s *store) SettleDecision(ctx context.Context, id, decisionID string,
	settle run.DecisionSettle) (bool, error) {
	if err := settle.Check(); err != nil {
		return false, err
	}
	settle.SanitizeText()
	const fence = " WHERE id=? AND status='" + run.StoredDeciding + "' AND decision_id=?"
	now := sqlutil.FormatTime(time.Now())
	var q string
	var args []any
	switch {
	case settle.Status == run.StatusRunning:
		q = "UPDATE runs SET status='running', decision_claim='', claimed_by=?, claimed_at=?, " +
			"started_at=COALESCE(NULLIF(started_at,''), ?)" + fence
		args = []any{settle.Owner, now, now, id, decisionID}
	case settle.Status == run.StatusPending:
		q = "UPDATE runs SET status='pending', decision_claim='', queued_at=?" + fence
		args = []any{now, id, decisionID}
	case settle.EndedAt.IsZero():
		q = "UPDATE runs SET status=?, decision_claim=''" + fence
		args = []any{string(settle.Status), id, decisionID}
	default:
		q = "UPDATE runs SET status=?, decision_claim='', error=?, ended_at=?" + fence
		args = []any{string(settle.Status), settle.Error, sqlutil.FormatTime(settle.EndedAt), id,
			decisionID}
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, fmt.Errorf("settle decision: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("settle decision: %w", err)
	}
	return n > 0, nil
}
