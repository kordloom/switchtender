package inventory

import (
	"context"
	"sort"
	"sync"
)

// memStore is an in-memory inventory Store guarded by a mutex.
type memStore struct {
	// mu guards inventories.
	mu sync.RWMutex
	// inventories maps inventory id to the stored inventory.
	inventories map[string]*Inventory
}

// NewMemStore returns an empty in-memory inventory Store.
func NewMemStore() Store {
	return &memStore{inventories: make(map[string]*Inventory)}
}

// Save inserts or replaces the inventory identified by i.ID.
func (m *memStore) Save(_ context.Context, i *Inventory) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *i
	cp.CredentialIDs = append([]string(nil), i.CredentialIDs...)
	m.inventories[i.ID] = &cp
	return nil
}

// Update changes an existing inventory's name and content, preserving its creation time, or
// returns ErrNotFound.
func (m *memStore) Update(_ context.Context, i *Inventory) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.inventories[i.ID]
	if !ok {
		return ErrNotFound
	}
	existing.Name = i.Name
	existing.Content = i.Content
	existing.CredentialIDs = append([]string(nil), i.CredentialIDs...)
	existing.ContentSource = i.ContentSource
	existing.ContentConfig = i.ContentConfig
	return nil
}

// Get returns the inventory with the given id, or ErrNotFound.
func (m *memStore) Get(_ context.Context, id string) (*Inventory, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	i, ok := m.inventories[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *i
	cp.CredentialIDs = append([]string(nil), i.CredentialIDs...)
	return &cp, nil
}

// List returns all inventories ordered by creation time, oldest first.
func (m *memStore) List(_ context.Context) ([]*Inventory, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Inventory, 0, len(m.inventories))
	for _, i := range m.inventories {
		cp := *i
		cp.CredentialIDs = append([]string(nil), i.CredentialIDs...)
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

// Delete removes the inventory with the given id, or returns ErrNotFound.
func (m *memStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.inventories[id]; !ok {
		return ErrNotFound
	}
	delete(m.inventories, id)
	return nil
}
