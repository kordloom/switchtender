package run

import (
	"context"
	"sort"
	"time"
)

// outcomeLedger is what the in-memory store keeps to answer which finished runs owe their outcome
// to the chain. The database stores mark a run in the write that makes it terminal. This store
// reads the same answer from the run itself: a finished top-level run owes its outcome unless it
// was stored already finished or its outcome was settled.
type outcomeLedger struct {
	// storedFinished holds the runs whose first save was already terminal. No process saw them
	// finish, so they owe nothing, the way a database insert of a finished run marks nothing.
	storedFinished map[string]bool
	// settled holds the runs whose outcome is on the chain.
	settled map[string]bool
	// owedSince holds when the ledger first found each run owing its outcome, which is the time
	// its age is measured from, as the database stores measure from their terminal write.
	owedSince map[string]time.Time
}

// noteSave records a run whose first save is already terminal. prev is the stored run before the
// save, nil for a new one.
func (l *outcomeLedger) noteSave(prev, next *Run) {
	if prev != nil || !next.Status.Terminal() {
		return
	}
	if l.storedFinished == nil {
		l.storedFinished = map[string]bool{}
	}
	l.storedFinished[next.ID] = true
}

// owes reports whether r owes its outcome.
func (l *outcomeLedger) owes(r *Run) bool {
	return r.ParentID == nil && r.Status.Terminal() && !l.storedFinished[r.ID] && !l.settled[r.ID]
}

// heldForOutcome reports whether retention must keep r because an outcome still owed to the chain
// is built from it: r owes its own outcome, or its parent owes one that records r. The janitor
// builds an outcome from the run's record, log, and summaries, and a parent's from each child's
// record and log, so removing any of them first would leave it to commit a record of something
// other than what ran. The caller holds m.mu.
func (m *memStore) heldForOutcome(r *Run) bool {
	if m.outcomes.owes(r) {
		return true
	}
	if r.ParentID == nil {
		return false
	}
	parent, ok := m.runs[*r.ParentID]
	return ok && m.outcomes.owes(parent)
}

// forget drops what the ledger holds for a run that no longer exists.
func (l *outcomeLedger) forget(id string) {
	delete(l.storedFinished, id)
	delete(l.settled, id)
	delete(l.owedSince, id)
}

// OwedOutcomes returns the finished top-level runs that owe their outcome, owed at least age since
// the ledger first found them owing, longest owed first.
func (m *memStore) OwedOutcomes(_ context.Context, age time.Duration, limit int) ([]string, error) {
	if limit < 1 {
		limit = 1
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	type owed struct {
		id    string
		since time.Time
	}
	now := time.Now()
	cutoff := now.Add(-age)
	var out []owed
	for _, r := range m.runs {
		if !m.outcomes.owes(r) {
			continue
		}
		since, seen := m.outcomes.owedSince[r.ID]
		if !seen {
			if m.outcomes.owedSince == nil {
				m.outcomes.owedSince = map[string]time.Time{}
			}
			since = now
			m.outcomes.owedSince[r.ID] = now
		}
		if since.After(cutoff) {
			continue
		}
		out = append(out, owed{id: r.ID, since: since})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].since.Equal(out[j].since) {
			return out[i].id < out[j].id
		}
		return out[i].since.Before(out[j].since)
	})
	ids := make([]string, 0, min(limit, len(out)))
	for _, o := range out {
		if len(ids) == limit {
			break
		}
		ids = append(ids, o.id)
	}
	return ids, nil
}

// OutcomeOwed reports whether the run owes its outcome.
func (m *memStore) OutcomeOwed(_ context.Context, id string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.runs[id]
	return ok && m.outcomes.owes(r), nil
}

// SettleOutcome records that the run's outcome is on the chain.
func (m *memStore) SettleOutcome(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok || !m.outcomes.owes(r) {
		return nil
	}
	if m.outcomes.settled == nil {
		m.outcomes.settled = map[string]bool{}
	}
	m.outcomes.settled[id] = true
	return nil
}
