package run

import (
	"context"
	"time"

	"github.com/kordloom/switchtender/internal/event"
)

// AppendLog appends raw output bytes to the run's log. Returns ErrNotFound if the run is absent.
func (m *memStore) AppendLog(_ context.Context, id string, p []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return ErrNotFound
	}
	if r.Status.Terminal() {
		// Fence a terminal run so a reclaimed-but-alive worker cannot append to a run that has ended.
		return nil
	}
	// Fence the accumulated size for the same reason the request body is capped: the total is what
	// fills a disk, and one chunk at a time is how it gets there.
	if len(m.logs[id]) >= MaxLogBytes {
		if r.Warning == "" {
			r.Warning = LogTruncatedWarning
		}
		return nil
	}
	m.logs[id] = append(m.logs[id], p...)
	return nil
}

// Log returns a copy of the run's captured output, or ErrNotFound.
func (m *memStore) Log(_ context.Context, id string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.runs[id]; !ok {
		return nil, ErrNotFound
	}
	out := make([]byte, len(m.logs[id]))
	copy(out, m.logs[id])
	return out, nil
}

// LogAfter returns the log bytes past afterSeq as a single chunk. The memory store's log sequence
// is the byte offset, so the returned chunk carries the total length as its Seq.
func (m *memStore) LogAfter(_ context.Context, id string, afterSeq int64, _ int) ([]LogChunk, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.runs[id]; !ok {
		return nil, ErrNotFound
	}
	buf := m.logs[id]
	if afterSeq < 0 {
		afterSeq = 0
	}
	if afterSeq >= int64(len(buf)) {
		return nil, nil
	}
	out := make([]byte, int64(len(buf))-afterSeq)
	copy(out, buf[afterSeq:])
	return []LogChunk{{Seq: int64(len(buf)), Data: out}}, nil
}

// LastLogSeq returns the byte length of the run's log, the memory store's log sequence.
func (m *memStore) LastLogSeq(_ context.Context, id string) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.runs[id]; !ok {
		return 0, ErrNotFound
	}
	return int64(len(m.logs[id])), nil
}

// AppendEvents appends structured events to the run. Returns ErrNotFound if the run is absent.
func (m *memStore) AppendEvents(_ context.Context, id string, events []event.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return ErrNotFound
	}
	if r.Status.Terminal() {
		// Fence a terminal run so a reclaimed-but-alive worker cannot stream events into it.
		return nil
	}
	m.events[id] = append(m.events[id], events...)
	return nil
}

// Events returns a copy of the run's structured events, or ErrNotFound.
func (m *memStore) Events(_ context.Context, id string) ([]event.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.runs[id]; !ok {
		return nil, ErrNotFound
	}
	out := make([]event.Event, len(m.events[id]))
	copy(out, m.events[id])
	return out, nil
}

// EventsAfter returns the run's events past afterSeq, capped at limit. The sequence is the
// one-based position, so it is monotonic within a run and usable as an opaque paging cursor.
func (m *memStore) EventsAfter(_ context.Context, id string, afterSeq int64, limit int) ([]event.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.runs[id]; !ok {
		return nil, ErrNotFound
	}
	var out []event.Event
	for i, e := range m.events[id] {
		seq := int64(i + 1)
		if seq <= afterSeq {
			continue
		}
		e.Seq = seq
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// LastEventSeq returns the one-based position of the run's last event, or zero when it has none.
func (m *memStore) LastEventSeq(_ context.Context, id string) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.runs[id]; !ok {
		return 0, ErrNotFound
	}
	return int64(len(m.events[id])), nil
}

// PurgeEventsBefore drops the events and logs of terminal runs created before cutoff, keeping the
// run records and their summaries. It returns how many runs were trimmed, counting only runs that
// actually held events or logs to remove.
func (m *memStore) PurgeEventsBefore(_ context.Context, cutoff time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	trimmed := 0
	for id, r := range m.runs {
		if !r.Status.Terminal() || !r.CreatedAt.Before(cutoff) {
			continue
		}
		if len(m.events[id]) == 0 && len(m.logs[id]) == 0 {
			continue
		}
		delete(m.events, id)
		delete(m.logs, id)
		trimmed++
	}
	return trimmed, nil
}

// PurgeRunsBefore deletes terminal runs created before cutoff along with their events and logs,
// keeping the per host and per task summaries. It returns how many runs were deleted.
func (m *memStore) PurgeRunsBefore(_ context.Context, cutoff time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	deleted := 0
	for id, r := range m.runs {
		if !r.Status.Terminal() || !r.CreatedAt.Before(cutoff) {
			continue
		}
		if r.IdempotencyKey != "" && m.byKey[r.IdempotencyKey] == id {
			delete(m.byKey, r.IdempotencyKey)
		}
		delete(m.runs, id)
		delete(m.events, id)
		delete(m.logs, id)
		deleted++
	}
	return deleted, nil
}
