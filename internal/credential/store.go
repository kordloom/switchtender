package credential

import (
	"context"
	"maps"
	"sort"
	"sync"
)

// memStore is an in-memory credential Store guarded by a mutex.
type memStore struct {
	// mu guards creds.
	mu sync.RWMutex
	// creds maps credential id to the stored credential.
	creds map[string]*Credential
}

// NewMemStore returns an empty in-memory credential Store.
func NewMemStore() Store {
	return &memStore{creds: make(map[string]*Credential)}
}

// Save inserts or replaces the credential identified by c.ID.
func (m *memStore) Save(_ context.Context, c *Credential) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *c
	cp.Settings = copySettings(c.Settings)
	m.creds[c.ID] = &cp
	return nil
}

// Update changes an existing credential's name, kind, and sealed secret, preserving its creation
// time, or returns ErrNotFound.
func (m *memStore) Update(_ context.Context, c *Credential) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.creds[c.ID]
	if !ok {
		return ErrNotFound
	}
	existing.Name = c.Name
	existing.Kind = c.Kind
	existing.Secret = c.Secret
	existing.Source = c.Source
	existing.OrgID = c.OrgID
	existing.TypeID = c.TypeID
	existing.VaultID = c.VaultID
	existing.Settings = copySettings(c.Settings)
	return nil
}

// Get returns the credential with the given id, or ErrNotFound.
func (m *memStore) Get(_ context.Context, id string) (*Credential, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.creds[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *c
	cp.Settings = copySettings(c.Settings)
	return &cp, nil
}

// List returns all credentials ordered by creation time, oldest first.
func (m *memStore) List(_ context.Context) ([]*Credential, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Credential, 0, len(m.creds))
	for _, c := range m.creds {
		cp := *c
		cp.Settings = copySettings(c.Settings)
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

// Delete removes the credential with the given id, or returns ErrNotFound.
func (m *memStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.creds[id]; !ok {
		return ErrNotFound
	}
	delete(m.creds, id)
	return nil
}

// copySettings returns an independent copy of a settings map, nil for empty, so a stored credential
// never shares map memory with a caller's value.
func copySettings(settings map[string]string) map[string]string {
	if len(settings) == 0 {
		return nil
	}
	return maps.Clone(settings)
}
