package grant

import (
	"context"
	"sort"
	"sync"
)

// memStore is an in-memory grant Store guarded by a mutex.
type memStore struct {
	// mu guards grants.
	mu sync.RWMutex
	// grants maps grant id to the stored grant.
	grants map[string]*Grant
}

// NewMemStore returns an empty in-memory grant Store.
func NewMemStore() Store {
	return &memStore{grants: make(map[string]*Grant)}
}

// Save inserts or replaces the grant identified by g.ID.
func (m *memStore) Save(_ context.Context, g *Grant) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *g
	m.grants[g.ID] = &cp
	return nil
}

// Get returns the grant with the given id, or ErrNotFound.
func (m *memStore) Get(_ context.Context, id string) (*Grant, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	g, ok := m.grants[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *g
	return &cp, nil
}

// List returns all grants ordered by creation time, oldest first.
func (m *memStore) List(_ context.Context) ([]*Grant, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Grant, 0, len(m.grants))
	for _, g := range m.grants {
		cp := *g
		out = append(out, &cp)
	}
	sortGrants(out)
	return out, nil
}

// Delete removes the grant with the given id, or returns ErrNotFound.
func (m *memStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.grants[id]; !ok {
		return ErrNotFound
	}
	delete(m.grants, id)
	return nil
}

// ForObject returns every grant on the given object, oldest first.
func (m *memStore) ForObject(_ context.Context, object string) ([]*Grant, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Grant
	for _, g := range m.grants {
		if g.Object == object {
			cp := *g
			out = append(out, &cp)
		}
	}
	sortGrants(out)
	return out, nil
}

// sortGrants orders grants by creation time, oldest first, ties broken by id.
func sortGrants(gs []*Grant) {
	sort.Slice(gs, func(i, j int) bool {
		if gs[i].CreatedAt.Equal(gs[j].CreatedAt) {
			return gs[i].ID < gs[j].ID
		}
		return gs[i].CreatedAt.Before(gs[j].CreatedAt)
	})
}
