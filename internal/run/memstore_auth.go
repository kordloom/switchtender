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

// PurgeRunAuth drops retained readability decisions that no longer govern anything.
//
// A decision may only go once nothing is left that it governs, so this checks every place a run id
// can still be held. The SQL backends check the same set as a list of tables; here they are the
// maps those tables correspond to.
func (m *memStore) PurgeRunAuth(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	referenced := make(map[string]bool)
	note := func(id string) {
		if id != "" {
			referenced[id] = true
		}
	}
	for id := range m.runs {
		note(id)
	}
	for id := range m.events {
		note(id)
	}
	for id := range m.logs {
		note(id)
	}
	for _, f := range m.facts {
		note(f.RunID)
	}
	for _, kept := range m.factsHistory {
		for _, f := range kept {
			note(f.RunID)
		}
	}
	// Keyed by run id, so the key is the reference. The RunID field on a summary is stamped by the
	// store and is not guaranteed to be set on a value held in memory, so reading it here would
	// miss references and strip decisions that are still needed.
	for id := range m.summaries {
		note(id)
	}
	for id := range m.tasks {
		note(id)
	}
	deleted := 0
	for id := range m.runAuth {
		if !referenced[id] {
			delete(m.runAuth, id)
			deleted++
		}
	}
	return deleted, nil
}
