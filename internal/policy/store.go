package policy

import (
	"context"
	"sort"
	"sync"
)

// memStore is an in-memory policy Store guarded by a mutex.
type memStore struct {
	// mu guards policies.
	mu sync.RWMutex
	// policies holds every stored policy by id.
	policies map[string]*Policy
}

// NewMemStore returns an empty in-memory policy Store.
func NewMemStore() Store {
	return &memStore{policies: make(map[string]*Policy)}
}

// Save stores a policy, inserting or replacing by id.
func (m *memStore) Save(_ context.Context, p *Policy) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *p
	m.policies[p.ID] = &cp
	return nil
}

// List returns every policy, oldest first.
func (m *memStore) List(_ context.Context) ([]*Policy, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Policy, 0, len(m.policies))
	for _, p := range m.policies {
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

// Delete removes a policy by id, returning ErrNotFound when it does not exist.
func (m *memStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.policies[id]; !ok {
		return ErrNotFound
	}
	delete(m.policies, id)
	return nil
}
