package run

import (
	"context"
	"sort"
	"sync"

	"github.com/dcadolph/yardmaster/internal/event"
)

// Store persists runs, their captured log output, and their structured events.
// Implementations must be safe for concurrent use.
type Store interface {
	// Save inserts or replaces the run identified by r.ID.
	Save(ctx context.Context, r *Run) error
	// Get returns the run with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*Run, error)
	// List returns all runs ordered by creation time, newest first.
	List(ctx context.Context) ([]*Run, error)
	// AppendLog appends raw output bytes to the run's log. Returns ErrNotFound if the run is absent.
	AppendLog(ctx context.Context, id string, p []byte) error
	// Log returns a copy of the run's captured output, or ErrNotFound.
	Log(ctx context.Context, id string) ([]byte, error)
	// AppendEvents appends structured events to the run. Returns ErrNotFound if the run is absent.
	AppendEvents(ctx context.Context, id string, events []event.Event) error
	// Events returns a copy of the run's structured events, or ErrNotFound.
	Events(ctx context.Context, id string) ([]event.Event, error)
}

// memStore is an in-memory Store backed by maps guarded by a read-write mutex.
type memStore struct {
	// mu guards runs, logs, and events.
	mu sync.RWMutex
	// runs maps run id to the stored run.
	runs map[string]*Run
	// logs maps run id to accumulated output bytes.
	logs map[string][]byte
	// events maps run id to accumulated structured events.
	events map[string][]event.Event
}

// NewMemStore returns an empty in-memory Store.
func NewMemStore() Store {
	return &memStore{
		runs:   make(map[string]*Run),
		logs:   make(map[string][]byte),
		events: make(map[string][]event.Event),
	}
}

// Save inserts or replaces the run identified by r.ID.
func (m *memStore) Save(_ context.Context, r *Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs[r.ID] = r.Clone()
	if _, ok := m.logs[r.ID]; !ok {
		m.logs[r.ID] = nil
	}
	return nil
}

// Get returns the run with the given id, or ErrNotFound.
func (m *memStore) Get(_ context.Context, id string) (*Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.runs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return r.Clone(), nil
}

// List returns all runs ordered by creation time, newest first.
func (m *memStore) List(_ context.Context) ([]*Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Run, 0, len(m.runs))
	for _, r := range m.runs {
		out = append(out, r.Clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// AppendLog appends raw output bytes to the run's log. Returns ErrNotFound if the run is absent.
func (m *memStore) AppendLog(_ context.Context, id string, p []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[id]; !ok {
		return ErrNotFound
	}
	m.logs[id] = append(m.logs[id], p...)
	return nil
}

// Log returns a copy of the run's captured output, or ErrNotFound.
func (m *memStore) Log(_ context.Context, id string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.runs[id]; !ok {
		return nil, ErrNotFound
	}
	out := make([]byte, len(m.logs[id]))
	copy(out, m.logs[id])
	return out, nil
}

// AppendEvents appends structured events to the run. Returns ErrNotFound if the run is absent.
func (m *memStore) AppendEvents(_ context.Context, id string, events []event.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[id]; !ok {
		return ErrNotFound
	}
	m.events[id] = append(m.events[id], events...)
	return nil
}

// Events returns a copy of the run's structured events, or ErrNotFound.
func (m *memStore) Events(_ context.Context, id string) ([]event.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.runs[id]; !ok {
		return nil, ErrNotFound
	}
	out := make([]event.Event, len(m.events[id]))
	copy(out, m.events[id])
	return out, nil
}
