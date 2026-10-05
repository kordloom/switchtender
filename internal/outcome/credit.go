package outcome

import (
	"context"
	"errors"
	"fmt"

	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/run"
)

// Credits reports whether a reader may credit the decision with id decisionID to subject, the
// held run or the workflow approval step it decided.
//
// A run stores the decision that won it. That decision is credited once it has settled, and no
// other: a decision still in flight has not taken effect, and any other decision named on the run
// lost, whether its record was left behind by a process that died before withdrawing it or its
// entry was written by an earlier release that appended before deciding. A run decided before
// decisions were claimed stores none, so every decision on it is credited as it always was, unless
// the run is still waiting, in which case none of them took effect.
func Credits(subject *run.Run, decisionID string) bool {
	if subject == nil {
		return false
	}
	if subject.DecisionID != "" {
		return subject.DecisionID == decisionID && !subject.InFlight()
	}
	return subject.Status != run.StatusPendingApproval
}

// EffectiveDecisions keeps the decision records that took effect and the corrections appended to
// them, in the order given. A record whose run or step no longer exists is kept, since nothing is
// left to say it lost.
func EffectiveDecisions(ctx context.Context, store run.Store,
	records []*decision.Record) ([]*decision.Record, error) {
	subjects := map[string]*run.Run{}
	kept := map[string]bool{}
	for _, rec := range records {
		if rec.Kind != decision.KindDecision {
			continue
		}
		id := rec.RunID
		if rec.StepRunID != "" {
			id = rec.StepRunID
		}
		subject, seen := subjects[id]
		if !seen {
			var err error
			subject, err = store.Get(ctx, id)
			if errors.Is(err, run.ErrNotFound) {
				kept[rec.ID] = true
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("read what decision %s decided: %w", rec.ID, err)
			}
			subjects[id] = subject
		}
		if Credits(subject, rec.ID) {
			kept[rec.ID] = true
		}
	}
	out := make([]*decision.Record, 0, len(records))
	for _, rec := range records {
		if (rec.Kind == decision.KindDecision && kept[rec.ID]) ||
			(rec.Kind != decision.KindDecision && kept[rec.DecisionID]) {
			out = append(out, rec)
		}
	}
	return out, nil
}
