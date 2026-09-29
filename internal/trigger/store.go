package trigger

import (
	"context"
	"sort"
	"sync"
)

// memStore is an in-memory trigger Store guarded by a mutex.
type memStore struct {
	// mu guards triggers.
	mu sync.RWMutex
	// triggers maps trigger id to the stored trigger.
	triggers map[string]*Trigger
}

// NewMemStore returns an empty in-memory trigger Store.
func NewMemStore() Store {
	return &memStore{triggers: make(map[string]*Trigger)}
}

// clone deep copies a trigger so callers cannot mutate stored state.
func clone(t *Trigger) *Trigger {
	cp := *t
	if t.LastFiredAt != nil {
		v := *t.LastFiredAt
		cp.LastFiredAt = &v
	}
	return &cp
}

// Save inserts or replaces the trigger identified by t.ID.
func (m *memStore) Save(_ context.Context, t *Trigger) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.triggers[t.ID] = clone(t)
	return nil
}

// Get returns the trigger with the given id, or ErrNotFound.
func (m *memStore) Get(_ context.Context, id string) (*Trigger, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.triggers[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(t), nil
}

// List returns all triggers ordered by creation time, oldest first.
func (m *memStore) List(_ context.Context) ([]*Trigger, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Trigger, 0, len(m.triggers))
	for _, t := range m.triggers {
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

// Delete removes the trigger with the given id, or returns ErrNotFound.
func (m *memStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.triggers[id]; !ok {
		return ErrNotFound
	}
	delete(m.triggers, id)
	return nil
}

// FindByTokenHash returns the trigger with the given token hash, or ErrNotFound.
func (m *memStore) FindByTokenHash(_ context.Context, hash string) (*Trigger, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, t := range m.triggers {
		if t.TokenHash == hash {
			return clone(t), nil
		}
	}
	return nil, ErrNotFound
}
