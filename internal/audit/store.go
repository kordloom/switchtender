package audit

import (
	"context"
	"sort"
	"sync"
)

// memStore is an in-memory audit Store guarded by a mutex.
type memStore struct {
	// mu guards entries.
	mu sync.RWMutex
	// entries holds every appended entry in chain order.
	entries []*Entry
}

// NewMemStore returns an empty in-memory audit Store.
func NewMemStore() Store {
	return &memStore{}
}

// Append records one entry, linking it to the current head so the chain stays intact.
func (m *memStore) Append(_ context.Context, e *Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var prev *Entry
	if n := len(m.entries); n > 0 {
		prev = m.entries[n-1]
	}
	cp := *e
	Link(prev, &cp)
	m.entries = append(m.entries, &cp)
	*e = cp
	return nil
}

// List returns up to limit entries, newest first.
func (m *memStore) List(_ context.Context, limit int) ([]*Entry, error) {
	if limit < 1 {
		limit = 1
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Entry, 0, len(m.entries))
	for _, e := range m.entries {
		cp := *e
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Seq == out[j].Seq {
			return out[i].ID > out[j].ID
		}
		return out[i].Seq > out[j].Seq
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Chain returns every entry in chain order, oldest first, for verification.
func (m *memStore) Chain(_ context.Context) ([]*Entry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Entry, 0, len(m.entries))
	for _, e := range m.entries {
		cp := *e
		out = append(out, &cp)
	}
	return out, nil
}
