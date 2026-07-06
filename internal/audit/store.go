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
	// entries holds every appended entry.
	entries []*Entry
}

// NewMemStore returns an empty in-memory audit Store.
func NewMemStore() Store {
	return &memStore{}
}

// Append records one entry.
func (m *memStore) Append(_ context.Context, e *Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *e
	m.entries = append(m.entries, &cp)
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
		if out[i].At.Equal(out[j].At) {
			return out[i].ID > out[j].ID
		}
		return out[i].At.After(out[j].At)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
