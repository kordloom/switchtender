package run

import (
	"context"
	"sort"
	"time"
)

// OwedEnds returns the top-level runs this store held unfinished that have since ended and are not
// settled. The in-memory store has no write to hang the mark on, so it notes a run's end the first
// time a sweep finds it and measures grace from there, oldest first.
func (m *memStore) OwedEnds(_ context.Context, grace time.Duration, limit int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	// owed is one end the sweep may announce.
	type owed struct {
		// id is the run.
		id string
		// at is when the store first found the run ended.
		at time.Time
	}
	var due []owed
	for id := range m.unended {
		r, ok := m.runs[id]
		if !ok {
			delete(m.unended, id)
			delete(m.endSeen, id)
			continue
		}
		if !r.Status.Terminal() {
			continue
		}
		at, seen := m.endSeen[id]
		if !seen {
			at = now
			m.endSeen[id] = at
		}
		if now.Sub(at) >= grace {
			due = append(due, owed{id: id, at: at})
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if !due[i].at.Equal(due[j].at) {
			return due[i].at.Before(due[j].at)
		}
		return due[i].id < due[j].id
	})
	out := []string{}
	for _, o := range due {
		if limit > 0 && len(out) == limit {
			break
		}
		out = append(out, o.id)
	}
	return out, nil
}

// SettleEnd marks a run's end announced.
func (m *memStore) SettleEnd(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.unended, id)
	delete(m.endSeen, id)
	return nil
}
