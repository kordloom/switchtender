package invsource

import (
	"context"
	"sort"
	"sync"
)

// memStore is an in-memory source Store guarded by a mutex.
type memStore struct {
	// mu guards sources.
	mu sync.RWMutex
	// sources maps source id to the stored source.
	sources map[string]*Source
}

// NewMemStore returns an empty in-memory source Store.
func NewMemStore() Store {
	return &memStore{sources: make(map[string]*Source)}
}

// clone deep copies a source so callers cannot mutate stored state.
func clone(s *Source) *Source {
	cp := *s
	if s.SyncedAt != nil {
		t := *s.SyncedAt
		cp.SyncedAt = &t
	}
	return &cp
}

// Save inserts or replaces the source identified by s.ID.
func (m *memStore) Save(_ context.Context, s *Source) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sources[s.ID] = clone(s)
	return nil
}

// Update changes an existing source's editable fields, leaving its backing inventory and sync
// state intact, or returns ErrNotFound.
func (m *memStore) Update(_ context.Context, s *Source) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.sources[s.ID]
	if !ok {
		return ErrNotFound
	}
	existing.Name = s.Name
	existing.Source = s.Source
	existing.CredentialID = s.CredentialID
	existing.ProjectID = s.ProjectID
	existing.UpdateOnLaunch = s.UpdateOnLaunch
	existing.SyncIntervalSeconds = s.SyncIntervalSeconds
	return nil
}

// Get returns the source with the given id, or ErrNotFound.
func (m *memStore) Get(_ context.Context, id string) (*Source, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sources[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(s), nil
}

// List returns all sources ordered by creation time, oldest first.
func (m *memStore) List(_ context.Context) ([]*Source, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Source, 0, len(m.sources))
	for _, s := range m.sources {
		out = append(out, clone(s))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// Delete removes the source with the given id, or returns ErrNotFound.
func (m *memStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sources[id]; !ok {
		return ErrNotFound
	}
	delete(m.sources, id)
	return nil
}
