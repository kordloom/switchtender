package org

import (
	"context"
	"sort"
	"sync"
)

// memStore is an in-memory org.Store for tests and single-node use.
type memStore struct {
	// mu guards orgs and members.
	mu sync.Mutex
	// orgs holds organizations by id.
	orgs map[string]*Org
	// members maps an org id to its members' roles by user id.
	members map[string]map[string]Role
}

// NewMemStore returns an in-memory org.Store.
func NewMemStore() Store {
	return &memStore{orgs: make(map[string]*Org), members: make(map[string]map[string]Role)}
}

// Save inserts or replaces the organization.
func (m *memStore) Save(_ context.Context, o *Org) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *o
	m.orgs[o.ID] = &cp
	return nil
}

// Get returns the organization with the given id, or ErrNotFound.
func (m *memStore) Get(_ context.Context, id string) (*Org, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.orgs[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *o
	return &cp, nil
}

// List returns all organizations ordered by creation time, oldest first.
func (m *memStore) List(_ context.Context) ([]*Org, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Org, 0, len(m.orgs))
	for _, o := range m.orgs {
		cp := *o
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

// Delete removes the organization with the given id and its memberships, or returns ErrNotFound.
func (m *memStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.orgs[id]; !ok {
		return ErrNotFound
	}
	delete(m.orgs, id)
	delete(m.members, id)
	return nil
}

// AddMember adds a user to an organization with a role, or updates an existing member's role.
func (m *memStore) AddMember(_ context.Context, orgID, userID string, role Role) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.members[orgID] == nil {
		m.members[orgID] = make(map[string]Role)
	}
	m.members[orgID][userID] = role
	return nil
}

// RemoveMember removes a user from an organization. Removing a non-member is a no-op.
func (m *memStore) RemoveMember(_ context.Context, orgID, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.members[orgID], userID)
	return nil
}

// Members returns an organization's members sorted by user id.
func (m *memStore) Members(_ context.Context, orgID string) ([]Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Member, 0, len(m.members[orgID]))
	for uid, role := range m.members[orgID] {
		out = append(out, Member{UserID: uid, Role: role})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserID < out[j].UserID })
	return out, nil
}

// OrgsForUser returns the organizations a user belongs to, sorted by org id.
func (m *memStore) OrgsForUser(_ context.Context, userID string) ([]Membership, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Membership
	for orgID, mem := range m.members {
		if role, ok := mem[userID]; ok {
			out = append(out, Membership{OrgID: orgID, Role: role})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OrgID < out[j].OrgID })
	return out, nil
}
