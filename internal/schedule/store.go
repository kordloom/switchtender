package schedule

import (
	"context"
	"sort"
	"sync"
)

// Store persists schedules. Implementations must be safe for concurrent use.
type Store interface {
	// Save inserts or replaces the schedule identified by s.ID.
	Save(ctx context.Context, s *Schedule) error
	// Get returns the schedule with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*Schedule, error)
	// List returns all schedules ordered by creation time, oldest first.
	List(ctx context.Context) ([]*Schedule, error)
	// Delete removes the schedule with the given id, or returns ErrNotFound.
	Delete(ctx context.Context, id string) error
}

// memStore is an in-memory Store guarded by a read-write mutex.
type memStore struct {
	// mu guards schedules.
	mu sync.RWMutex
	// schedules maps schedule id to the stored schedule.
	schedules map[string]*Schedule
}

// NewMemStore returns an empty in-memory Store.
func NewMemStore() Store {
	return &memStore{schedules: make(map[string]*Schedule)}
}

// Save inserts or replaces the schedule identified by s.ID.
func (m *memStore) Save(_ context.Context, s *Schedule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.schedules[s.ID] = s.Clone()
	return nil
}

// Get returns the schedule with the given id, or ErrNotFound.
func (m *memStore) Get(_ context.Context, id string) (*Schedule, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.schedules[id]
	if !ok {
		return nil, ErrNotFound
	}
	return s.Clone(), nil
}

// List returns all schedules ordered by creation time, oldest first.
func (m *memStore) List(_ context.Context) ([]*Schedule, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Schedule, 0, len(m.schedules))
	for _, s := range m.schedules {
		out = append(out, s.Clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// Delete removes the schedule with the given id, or returns ErrNotFound.
func (m *memStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.schedules[id]; !ok {
		return ErrNotFound
	}
	delete(m.schedules, id)
	return nil
}
