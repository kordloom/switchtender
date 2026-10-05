package factcache

import (
	"bytes"
	"context"
	"sort"
	"sync"
)

// memKey identifies one host's entry in one inventory.
type memKey struct {
	// inventory is the stored inventory id.
	inventory string
	// host is the inventory host name.
	host string
}

// memStore is an in-memory fact cache Store guarded by a mutex.
type memStore struct {
	// mu guards entries.
	mu sync.RWMutex
	// entries maps an inventory and host to the stored entry.
	entries map[memKey]Entry
}

// NewMemStore returns an empty in-memory fact cache Store.
func NewMemStore() Store {
	return &memStore{entries: make(map[memKey]Entry)}
}

// cloneEntry copies an entry so a caller cannot reach stored state through its facts.
func cloneEntry(e Entry, withFacts bool) Entry {
	if withFacts {
		e.Facts = bytes.Clone(e.Facts)
	} else {
		e.Facts = nil
	}
	return e
}

// SaveFacts validates every entry and then writes each one that was not gathered before the facts
// already stored for its host.
func (m *memStore) SaveFacts(_ context.Context, entries []Entry) error {
	for _, e := range entries {
		if err := Validate(e); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range entries {
		e = cloneEntry(e, true)
		e.Bytes = len(e.Facts)
		e.ModifiedAt = e.ModifiedAt.UTC()
		key := memKey{e.InventoryID, e.Host}
		if held, ok := m.entries[key]; ok && held.ModifiedAt.After(e.ModifiedAt) {
			continue
		}
		m.entries[key] = e
	}
	return nil
}

// Facts returns one host's entry with its fact document, or ErrNotFound.
func (m *memStore) Facts(_ context.Context, inventoryID, host string) (*Entry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.entries[memKey{inventoryID, host}]
	if !ok {
		return nil, ErrNotFound
	}
	out := cloneEntry(e, true)
	return &out, nil
}

// List returns an inventory's entries ordered by host.
func (m *memStore) List(_ context.Context, inventoryID string, withFacts bool) ([]Entry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Entry
	for k, e := range m.entries {
		if k.inventory == inventoryID {
			out = append(out, cloneEntry(e, withFacts))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

// Clear removes the named hosts' facts from an inventory.
func (m *memStore) Clear(_ context.Context, inventoryID string, hosts ...string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, h := range hosts {
		k := memKey{inventoryID, h}
		if _, ok := m.entries[k]; ok {
			delete(m.entries, k)
			n++
		}
	}
	return n, nil
}

// ClearInventory removes every host's facts from an inventory.
func (m *memStore) ClearInventory(_ context.Context, inventoryID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k := range m.entries {
		if k.inventory == inventoryID {
			delete(m.entries, k)
			n++
		}
	}
	return n, nil
}
