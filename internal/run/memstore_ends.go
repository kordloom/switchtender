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

// noteStatus marks a top-level run's start or hold owed when a write moved it from status from into
// running or into pending_approval, the in-memory stand-in for the triggers the database stores
// keep. A write that leaves the status where it was owes nothing. It runs under m.mu.
func (m *memStore) noteStatus(r *Run, from Status) {
	if r.ParentID != nil || r.Status == from {
		return
	}
	event := ""
	switch r.Status {
	case StatusRunning:
		event = OwedStart
	case StatusPendingApproval:
		event = OwedHold
	default:
		return
	}
	owed := OwedEvent{RunID: r.ID, Event: event}
	if _, ok := m.owedEvents[owed]; !ok {
		m.owedEvents[owed] = time.Now()
	}
}

// OwedEvents returns the starts and holds owed for at least grace, oldest first. A mark whose run
// is gone, purged by retention, has nothing left to announce and is dropped.
func (m *memStore) OwedEvents(_ context.Context, grace time.Duration, limit int) ([]OwedEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	// owed is one mark the sweep may announce.
	type owed struct {
		// event is the run and what it owes.
		event OwedEvent
		// at is when it became owed.
		at time.Time
	}
	var due []owed
	for ev, at := range m.owedEvents {
		if _, ok := m.runs[ev.RunID]; !ok {
			delete(m.owedEvents, ev)
			continue
		}
		if now.Sub(at) >= grace {
			due = append(due, owed{event: ev, at: at})
		}
	}
	sort.Slice(due, func(i, j int) bool {
		switch {
		case !due[i].at.Equal(due[j].at):
			return due[i].at.Before(due[j].at)
		case due[i].event.RunID != due[j].event.RunID:
			return due[i].event.RunID < due[j].event.RunID
		}
		return due[i].event.Event < due[j].event.Event
	})
	out := []OwedEvent{}
	for _, o := range due {
		if limit > 0 && len(out) == limit {
			break
		}
		out = append(out, o.event)
	}
	return out, nil
}

// SettleEvent marks a run's start or hold announced.
func (m *memStore) SettleEvent(_ context.Context, id, event string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.owedEvents, OwedEvent{RunID: id, Event: event})
	return nil
}
