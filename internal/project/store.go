package project

import (
	"context"
	"sort"
	"sync"
)

// memStore is an in-memory project Store guarded by a mutex.
type memStore struct {
	// mu guards projects.
	mu sync.RWMutex
	// projects maps project id to the stored project.
	projects map[string]*Project
}

// NewMemStore returns an empty in-memory project Store.
func NewMemStore() Store {
	return &memStore{projects: make(map[string]*Project)}
}

// Save inserts or replaces the project identified by p.ID.
func (m *memStore) Save(_ context.Context, p *Project) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *p
	m.projects[p.ID] = &cp
	return nil
}

// Update changes an existing project's mutable fields, preserving its creation time, or returns
// ErrNotFound.
func (m *memStore) Update(_ context.Context, p *Project) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.projects[p.ID]
	if !ok {
		return ErrNotFound
	}
	cp := *p
	cp.CreatedAt = existing.CreatedAt
	m.projects[p.ID] = &cp
	return nil
}

// Get returns the project with the given id, or ErrNotFound.
func (m *memStore) Get(_ context.Context, id string) (*Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.projects[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *p
	return &cp, nil
}

// List returns all projects ordered by creation time, oldest first.
func (m *memStore) List(_ context.Context) ([]*Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Project, 0, len(m.projects))
	for _, p := range m.projects {
		cp := *p
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// Delete removes the project with the given id, or returns ErrNotFound.
func (m *memStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[id]; !ok {
		return ErrNotFound
	}
	delete(m.projects, id)
	return nil
}
