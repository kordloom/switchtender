package template

import (
	"context"
	"maps"
	"sort"
	"sync"
)

// memStore is an in-memory template Store guarded by a mutex.
type memStore struct {
	// mu guards templates.
	mu sync.RWMutex
	// templates maps template id to the stored template.
	templates map[string]*Template
}

// NewMemStore returns an empty in-memory template Store.
func NewMemStore() Store {
	return &memStore{templates: make(map[string]*Template)}
}

// clone deep copies a template so callers cannot mutate stored state.
func clone(t *Template) *Template {
	cp := *t
	cp.CredentialIDs = append([]string(nil), t.CredentialIDs...)
	cp.SelectableCredentialIDs = append([]string(nil), t.SelectableCredentialIDs...)
	cp.ExtraVars = maps.Clone(t.ExtraVars)
	cp.Survey = append([]SurveyField(nil), t.Survey...)
	return &cp
}

// Save inserts or replaces the template identified by t.ID.
func (m *memStore) Save(_ context.Context, t *Template) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.templates[t.ID] = clone(t)
	return nil
}

// Update changes an existing template's fields, preserving its creation time, or returns
// ErrNotFound.
func (m *memStore) Update(_ context.Context, t *Template) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.templates[t.ID]
	if !ok {
		return ErrNotFound
	}
	cp := clone(t)
	cp.CreatedAt = existing.CreatedAt
	m.templates[t.ID] = cp
	return nil
}

// Get returns the template with the given id, or ErrNotFound.
func (m *memStore) Get(_ context.Context, id string) (*Template, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.templates[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(t), nil
}

// List returns all templates ordered by creation time, oldest first.
func (m *memStore) List(_ context.Context) ([]*Template, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Template, 0, len(m.templates))
	for _, t := range m.templates {
		out = append(out, clone(t))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// Delete removes the template with the given id, or returns ErrNotFound.
func (m *memStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.templates[id]; !ok {
		return ErrNotFound
	}
	delete(m.templates, id)
	return nil
}
