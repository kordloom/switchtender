package user

import (
	"context"
	"sort"
	"sync"
)

// memStore is an in-memory user Store guarded by a mutex.
type memStore struct {
	// mu guards users.
	mu sync.RWMutex
	// users maps user id to the stored user.
	users map[string]*User
}

// NewMemStore returns an empty in-memory user Store.
func NewMemStore() Store {
	return &memStore{users: make(map[string]*User)}
}

// Save inserts or replaces the user identified by u.ID.
func (m *memStore) Save(_ context.Context, u *User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *u
	m.users[u.ID] = &cp
	return nil
}

// Get returns the user with the given id, or ErrNotFound.
func (m *memStore) Get(_ context.Context, id string) (*User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.users[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *u
	return &cp, nil
}

// FindByUsername returns the user with the given username, or ErrNotFound.
func (m *memStore) FindByUsername(_ context.Context, username string) (*User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, u := range m.users {
		if u.Username == username {
			cp := *u
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

// List returns all users ordered by creation time, oldest first.
func (m *memStore) List(_ context.Context) ([]*User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*User, 0, len(m.users))
	for _, u := range m.users {
		cp := *u
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

// Delete removes the user with the given id, or returns ErrNotFound.
func (m *memStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[id]; !ok {
		return ErrNotFound
	}
	delete(m.users, id)
	return nil
}
