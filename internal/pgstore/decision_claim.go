package pgstore

import (
	"context"
	"fmt"

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
UPDATE runs SET status=$1, decision_id=$2, decision_claim=$3
WHERE id=$4 AND status=$5 AND claimed_by='' AND cancel_requested=0`,
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
	const fence = " WHERE id=$1 AND status='" + run.StoredDeciding + "' AND decision_id=$2"
	var q string
	args := []any{id, decisionID}
	switch {
	case settle.Status == run.StatusRunning:
		q = `UPDATE runs SET status='running', decision_claim='', claimed_by=$3, claimed_at=` +
			pgNowText + `, started_at=COALESCE(NULLIF(started_at,''), ` + pgNowText + `)` + fence
		args = append(args, settle.Owner)
	case settle.Status == run.StatusPending:
		q = `UPDATE runs SET status='pending', decision_claim='', queued_at=` + pgNowText + fence
	case settle.EndedAt.IsZero():
		q = `UPDATE runs SET status=$3, decision_claim=''` + fence
		args = append(args, string(settle.Status))
	default:
		q = `UPDATE runs SET status=$3, decision_claim='', error=$4, ended_at=$5` + fence
		args = append(args, string(settle.Status), settle.Error,
			sqlutil.FormatTime(settle.EndedAt))
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
