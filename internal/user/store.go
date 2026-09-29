package user

import (
	"context"
	"slices"
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

// clone returns a deep copy of u, so the store never hands out a slice a caller could mutate in
// place and never adopts one a caller keeps a reference to.
func clone(u *User) *User {
	cp := *u
	cp.Links = slices.Clone(u.Links)
	return &cp
}

// Save inserts or replaces the user identified by u.ID.
func (m *memStore) Save(_ context.Context, u *User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users[u.ID] = clone(u)
	return nil
}

// Update changes an existing user's username, role, password hash, and profile, preserving the
// creation time, or returns ErrNotFound.
func (m *memStore) Update(_ context.Context, u *User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.users[u.ID]
	if !ok {
		return ErrNotFound
	}
	existing.Username = u.Username
	existing.Role = u.Role
	existing.PasswordHash = u.PasswordHash
	existing.FullName = u.FullName
	existing.Email = u.Email
	existing.Phone = u.Phone
	existing.Title = u.Title
	existing.Links = slices.Clone(u.Links)
	existing.Notes = u.Notes
	return nil
}

// lastAdminLocked reports whether id is the only administrator. The caller holds the write lock, so
// nothing can add or remove an admin between this and the change it guards.
func (m *memStore) lastAdminLocked(id string) bool {
	target, others := false, 0
	for _, u := range m.users {
		if u.Role != RoleAdmin {
			continue
		}
		if u.ID == id {
			target = true
		} else {
			others++
		}
	}
	return target && others == 0
}

// DeleteUnlessLastAdmin removes the user unless doing so would leave no administrator.
func (m *memStore) DeleteUnlessLastAdmin(_ context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[id]; !ok {
		return false, ErrNotFound
	}
	if m.lastAdminLocked(id) {
		return false, nil
	}
	delete(m.users, id)
	return true, nil
}

// UpdateUnlessLastAdmin applies the update unless it would demote the only administrator.
func (m *memStore) UpdateUnlessLastAdmin(_ context.Context, u *User) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.users[u.ID]
	if !ok {
		return false, ErrNotFound
	}
	if u.Role != RoleAdmin && m.lastAdminLocked(u.ID) {
		return false, nil
	}
	existing.Username = u.Username
	existing.Role = u.Role
	existing.PasswordHash = u.PasswordHash
	existing.FullName = u.FullName
	existing.Email = u.Email
	existing.Phone = u.Phone
	existing.Title = u.Title
	existing.Links = slices.Clone(u.Links)
	existing.Notes = u.Notes
	return true, nil
}

// Get returns the user with the given id, or ErrNotFound.
func (m *memStore) Get(_ context.Context, id string) (*User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.users[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(u), nil
}

// FindByUsername returns the user with the given username, or ErrNotFound.
func (m *memStore) FindByUsername(_ context.Context, username string) (*User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, u := range m.users {
		if u.Username == username {
			return clone(u), nil
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
		out = append(out, clone(u))
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
