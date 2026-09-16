package run

import (
	"context"
	"slices"
)

// RunAuthFor returns what decides a run's readability, from the run while it exists and from the
// retained decision once a purge has deleted it.
func (m *memStore) RunAuthFor(_ context.Context, id string) (*RunAuth, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if r, ok := m.runs[id]; ok {
		return AuthOf(r), nil
	}
	if auth, ok := m.runAuth[id]; ok {
		cp := auth
		cp.CredentialIDs = slices.Clone(auth.CredentialIDs)
		return &cp, nil
	}
	return nil, ErrNotFound
}
