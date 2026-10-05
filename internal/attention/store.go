package attention

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"
)

// WorkerRetention is how long a worker that stopped reporting is remembered. It only has to outlive
// the longest wait a dashboard explains, which is the time since the last worker for a queue went
// away, so a day is ample, and it keeps the table the size of the fleet rather than of its history.
const WorkerRetention = 24 * time.Hour

// AlertRetention is how long a raised alert is remembered. An alert is raised once per condition
// and the record only has to outlive that condition, so a week bounds the table without any
// condition alerting twice.
const AlertRetention = 7 * 24 * time.Hour

// Worker is one executor's latest report that it is polling for work.
type Worker struct {
	// Owner is the executor's lease name, the name it stamps on the runs it claims.
	Owner string `json:"owner"`
	// Queues are the queues it serves. The default queue is the empty name.
	Queues []string `json:"queues"`
	// Slots is how many runs it executes at once, zero when it did not say.
	Slots int `json:"slots,omitempty"`
	// FirstSeen is when it first reported.
	FirstSeen time.Time `json:"first_seen"`
	// LastSeen is when it last reported.
	LastSeen time.Time `json:"last_seen"`
}

// Serves reports whether the worker takes runs from queue. A worker that names no queues serves the
// default queue, the same reading a claim gives an empty list.
func (w Worker) Serves(queue string) bool {
	if len(w.Queues) == 0 {
		return queue == ""
	}
	return slices.Contains(w.Queues, queue)
}

// PresenceRecorder records which executors are polling for work. A dispatcher reports itself
// through it, and the relay records the workers that reach the control node across it.
type PresenceRecorder interface {
	// NoteWorker records that owner is polling for work now, serving queues with slots runs at
	// once, under the store's own clock so every replica measures absence the same way. It also
	// forgets workers silent for longer than WorkerRetention.
	NoteWorker(ctx context.Context, owner string, queues []string, slots int) error
	// TouchWorker records that owner is still reporting, leaving the queues and slots it noted
	// alone. It does nothing for an owner never noted, since a heartbeat says nothing about the
	// queues a worker serves.
	TouchWorker(ctx context.Context, owner string) error
}

// Store records which executors are polling for work and which alerts were raised. Implementations
// must be safe for concurrent use, and both database stores share their table with every process
// on the database.
type Store interface {
	PresenceRecorder
	// Workers returns every executor seen at or after since, ordered by owner.
	Workers(ctx context.Context, since time.Time) ([]Worker, error)
	// ClaimAlert records key the first time any process raises it and reports whether this call was
	// the one that recorded it, so of several replicas evaluating the same condition exactly one
	// alerts. It forgets keys older than AlertRetention.
	ClaimAlert(ctx context.Context, key string) (bool, error)
	// Now returns the clock the store stamps worker reports with.
	Now(ctx context.Context) (time.Time, error)
}

// memStore is an in-memory Store for tests and single-process tools.
type memStore struct {
	// mu guards workers and alerts.
	mu sync.Mutex
	// workers maps an owner to its latest report.
	workers map[string]Worker
	// alerts maps a raised alert key to when it was raised.
	alerts map[string]time.Time
	// now reads the clock reports are stamped with.
	now func() time.Time
}

// NewMemStore returns an empty in-memory Store on the wall clock.
func NewMemStore() Store {
	return NewMemStoreAt(time.Now)
}

// NewMemStoreAt returns an empty in-memory Store whose clock is now, so a test can move time.
func NewMemStoreAt(now func() time.Time) Store {
	if now == nil {
		now = time.Now
	}
	return &memStore{workers: map[string]Worker{}, alerts: map[string]time.Time{}, now: now}
}

// NoteWorker records a report, keeping the first time the owner was seen.
func (m *memStore) NoteWorker(_ context.Context, owner string, queues []string, slots int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for name, w := range m.workers {
		if now.Sub(w.LastSeen) > WorkerRetention {
			delete(m.workers, name)
		}
	}
	w, ok := m.workers[owner]
	if !ok {
		w = Worker{Owner: owner, FirstSeen: now}
	}
	w.Queues = append([]string(nil), queues...)
	w.Slots = slots
	w.LastSeen = now
	m.workers[owner] = w
	return nil
}

// TouchWorker refreshes an owner already noted.
func (m *memStore) TouchWorker(_ context.Context, owner string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.workers[owner]
	if !ok {
		return nil
	}
	w.LastSeen = m.now()
	m.workers[owner] = w
	return nil
}

// Workers returns the owners seen at or after since, ordered by owner.
func (m *memStore) Workers(_ context.Context, since time.Time) ([]Worker, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Worker{}
	for _, w := range m.workers {
		if w.LastSeen.Before(since) {
			continue
		}
		w.Queues = append([]string(nil), w.Queues...)
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Owner < out[j].Owner })
	return out, nil
}

// ClaimAlert records key once.
func (m *memStore) ClaimAlert(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for k, at := range m.alerts {
		if now.Sub(at) > AlertRetention {
			delete(m.alerts, k)
		}
	}
	if _, ok := m.alerts[key]; ok {
		return false, nil
	}
	m.alerts[key] = now
	return true, nil
}

// Now returns the store's clock.
func (m *memStore) Now(context.Context) (time.Time, error) {
	return m.now(), nil
}
