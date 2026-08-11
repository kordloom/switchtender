package auth

import (
	"context"
	"sort"
	"sync"
	"time"
)

// memStore is an in-memory token Store guarded by a mutex.
type memStore struct {
	// mu guards tokens.
	mu sync.RWMutex
	// tokens maps token id to the stored token.
	tokens map[string]*Token
}

// NewMemStore returns an empty in-memory token Store.
func NewMemStore() Store {
	return &memStore{tokens: make(map[string]*Token)}
}

// Save inserts or replaces the token identified by t.ID.
func (m *memStore) Save(_ context.Context, t *Token) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *t
	m.tokens[t.ID] = &cp
	return nil
}

// List returns all tokens ordered by creation time, oldest first.
func (m *memStore) List(_ context.Context) ([]*Token, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Token, 0, len(m.tokens))
	for _, t := range m.tokens {
		cp := *t
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

// Touch records a token's last use without creating one. A revoke that lands first wins: the token
// is simply absent and there is nothing to update.
func (m *memStore) Touch(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tokens[id]; ok {
		cp := at
		t.LastUsedAt = &cp
	}
	return nil
}

// Delete removes the token with the given id, or returns ErrNotFound.
func (m *memStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tokens[id]; !ok {
		return ErrNotFound
	}
	delete(m.tokens, id)
	return nil
}

// FindByHash returns the token with the given hash, or ErrNotFound.
func (m *memStore) FindByHash(_ context.Context, hash string) (*Token, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, t := range m.tokens {
		if t.Hash == hash {
			cp := *t
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

// Count returns how many tokens exist.
func (m *memStore) Count(_ context.Context) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.tokens), nil
}
