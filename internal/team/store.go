package team

import (
	"context"
	"sort"
	"sync"
)

// memStore is an in-memory team Store guarded by a mutex.
type memStore struct {
	// mu guards teams and members.
	mu sync.RWMutex
	// teams maps team id to the stored team.
	teams map[string]*Team
	// members maps team id to the set of member user ids.
	members map[string]map[string]bool
}

// NewMemStore returns an empty in-memory team Store.
func NewMemStore() Store {
	return &memStore{teams: make(map[string]*Team), members: make(map[string]map[string]bool)}
}

// Save inserts or replaces the team identified by t.ID.
func (m *memStore) Save(_ context.Context, t *Team) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *t
	m.teams[t.ID] = &cp
	return nil
}

// Get returns the team with the given id, or ErrNotFound.
func (m *memStore) Get(_ context.Context, id string) (*Team, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.teams[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *t
	return &cp, nil
}

// List returns all teams ordered by creation time, oldest first.
func (m *memStore) List(_ context.Context) ([]*Team, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Team, 0, len(m.teams))
	for _, t := range m.teams {
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

// Delete removes the team with the given id and its memberships, or returns ErrNotFound.
func (m *memStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.teams[id]; !ok {
		return ErrNotFound
	}
	delete(m.teams, id)
	delete(m.members, id)
	return nil
}

// AddMember adds a user to a team. Adding an existing member is a no-op.
func (m *memStore) AddMember(_ context.Context, teamID, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// The team has to exist, the way both SQL backends require on a foreign key.
	if _, ok := m.teams[teamID]; !ok {
		return ErrNotFound
	}
	if m.members[teamID] == nil {
		m.members[teamID] = make(map[string]bool)
	}
	m.members[teamID][userID] = true
	return nil
}

// RemoveMember removes a user from a team. Removing a non-member is a no-op.
func (m *memStore) RemoveMember(_ context.Context, teamID, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.members[teamID], userID)
	return nil
}

// Members returns the user ids in a team, sorted.
func (m *memStore) Members(_ context.Context, teamID string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.members[teamID]))
	for userID := range m.members[teamID] {
		out = append(out, userID)
	}
	sort.Strings(out)
	return out, nil
}

// TeamsForUser returns the ids of the teams a user belongs to, sorted.
func (m *memStore) TeamsForUser(_ context.Context, userID string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []string
	for teamID, set := range m.members {
		if set[userID] {
			out = append(out, teamID)
		}
	}
	sort.Strings(out)
	return out, nil
}
