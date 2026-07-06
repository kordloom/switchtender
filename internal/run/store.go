package run

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/dcadolph/yardmaster/internal/event"
)

// Store persists runs, their captured log output, and their structured events.
// Implementations must be safe for concurrent use.
type Store interface {
	// Save inserts or replaces the run identified by r.ID.
	Save(ctx context.Context, r *Run) error
	// Get returns the run with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*Run, error)
	// List returns top-level runs, excluding shard runs, ordered by creation time, newest first.
	List(ctx context.Context) ([]*Run, error)
	// Shards returns the shard runs of a parent ordered by shard index.
	Shards(ctx context.Context, parentID string) ([]*Run, error)
	// Steps returns the pipeline step runs of a parent ordered by step index.
	Steps(ctx context.Context, parentID string) ([]*Run, error)
	// NonTerminal returns all runs, including shards, that are not in a terminal state.
	NonTerminal(ctx context.Context) ([]*Run, error)
	// Claim leases the oldest unclaimed pending executable run to owner and returns it, or
	// ErrNonePending when nothing is waiting. Plain runs, shard children, and pipeline step
	// children are executable; split and pipeline parents are coordination records and are not.
	// The claim must be atomic across processes.
	Claim(ctx context.Context, owner string) (*Run, error)
	// Heartbeat renews owner's lease on a run. It returns ErrNotFound when the run is gone or the
	// lease is no longer held by owner.
	Heartbeat(ctx context.Context, id, owner string) error
	// ReclaimStale requeues pending runs whose lease renewal is older than cutoff and marks stale
	// running runs interrupted, returning how many rows changed. It sweeps up after dead workers.
	ReclaimStale(ctx context.Context, cutoff time.Time) (int, error)
	// RequestCancel marks the run so whichever process holds it stops it, or ErrNotFound.
	RequestCancel(ctx context.Context, id string) error
	// SaveHostSummary replaces the stored per host summaries for a run.
	SaveHostSummary(ctx context.Context, runID string, summaries []HostSummary) error
	// FleetHealth ranks hosts by failures over their most recent window runs, worst first.
	FleetHealth(ctx context.Context, window int) ([]HostHealth, error)
	// HostCosts returns each host's average recorded duration in seconds over its most recent
	// window runs, for balancing splits by past cost.
	HostCosts(ctx context.Context, window int) (map[string]float64, error)
	// HostHistory returns a host's most recent per run summaries, newest first, with run ids.
	HostHistory(ctx context.Context, host string, limit int) ([]HostSummary, error)
	// SaveTaskSummary replaces the stored per task summaries for a run.
	SaveTaskSummary(ctx context.Context, runID string, summaries []TaskSummary) error
	// TaskTrends aggregates each task's durations over its most recent window runs.
	TaskTrends(ctx context.Context, window int) ([]TaskTrend, error)
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
	// summaries maps run id to its per host outcome summaries.
	summaries map[string][]HostSummary
	// tasks maps run id to its per task duration summaries.
	tasks map[string][]TaskSummary
}

// NewMemStore returns an empty in-memory Store.
func NewMemStore() Store {
	return &memStore{
		runs:      make(map[string]*Run),
		logs:      make(map[string][]byte),
		events:    make(map[string][]event.Event),
		summaries: make(map[string][]HostSummary),
		tasks:     make(map[string][]TaskSummary),
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

// List returns top-level runs, excluding shard runs, ordered by creation time, newest first.
func (m *memStore) List(_ context.Context) ([]*Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Run, 0, len(m.runs))
	for _, r := range m.runs {
		if r.ParentID != nil {
			continue
		}
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

// Shards returns the shard runs of a parent ordered by shard index.
func (m *memStore) Shards(_ context.Context, parentID string) ([]*Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Run
	for _, r := range m.runs {
		if r.ParentID != nil && *r.ParentID == parentID {
			out = append(out, r.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return shardIndex(out[i]) < shardIndex(out[j])
	})
	return out, nil
}

// shardIndex returns a run's shard index for ordering, or a large value when unset.
func shardIndex(r *Run) int {
	if r.ShardIndex == nil {
		return 1 << 30
	}
	return *r.ShardIndex
}

// Steps returns the pipeline step runs of a parent ordered by step index.
func (m *memStore) Steps(_ context.Context, parentID string) ([]*Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Run
	for _, r := range m.runs {
		if r.ParentID != nil && *r.ParentID == parentID {
			out = append(out, r.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if stepIndex(out[i]) != stepIndex(out[j]) {
			return stepIndex(out[i]) < stepIndex(out[j])
		}
		return out[i].Attempt < out[j].Attempt
	})
	return out, nil
}

// stepIndex returns a run's step index for ordering, or a large value when unset.
func stepIndex(r *Run) int {
	if r.StepIndex == nil {
		return 1 << 30
	}
	return *r.StepIndex
}

// NonTerminal returns all runs, including shards, that are not in a terminal state.
func (m *memStore) NonTerminal(_ context.Context) ([]*Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Run
	for _, r := range m.runs {
		if !r.Status.Terminal() {
			out = append(out, r.Clone())
		}
	}
	return out, nil
}

// Claim leases the oldest unclaimed pending top-level plain run to owner and returns it.
func (m *memStore) Claim(_ context.Context, owner string) (*Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var oldest *Run
	for _, r := range m.runs {
		if r.Status != StatusPending || r.ClaimedBy != "" || r.Kind != "" {
			continue
		}
		if oldest == nil || r.CreatedAt.Before(oldest.CreatedAt) ||
			(r.CreatedAt.Equal(oldest.CreatedAt) && r.ID < oldest.ID) {
			oldest = r
		}
	}
	if oldest == nil {
		return nil, ErrNonePending
	}
	now := time.Now()
	oldest.ClaimedBy = owner
	oldest.ClaimedAt = &now
	return oldest.Clone(), nil
}

// Heartbeat renews owner's lease on a run.
func (m *memStore) Heartbeat(_ context.Context, id, owner string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok || r.ClaimedBy != owner {
		return ErrNotFound
	}
	now := time.Now()
	r.ClaimedAt = &now
	return nil
}

// ReclaimStale requeues stale claimed pending runs and interrupts stale running runs.
func (m *memStore) ReclaimStale(_ context.Context, cutoff time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := 0
	for _, r := range m.runs {
		if r.ClaimedBy == "" || r.ClaimedAt == nil || !r.ClaimedAt.Before(cutoff) {
			continue
		}
		switch r.Status {
		case StatusPending:
			r.ClaimedBy = ""
			r.ClaimedAt = nil
			changed++
		case StatusRunning:
			now := time.Now()
			r.Status = StatusInterrupted
			r.EndedAt = &now
			if r.Error == "" {
				r.Error = "interrupted: executor lease expired"
			}
			changed++
		}
	}
	return changed, nil
}

// RequestCancel marks the run so whichever process holds it stops it.
func (m *memStore) RequestCancel(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return ErrNotFound
	}
	r.CancelRequested = true
	return nil
}

// SaveHostSummary replaces the stored per host summaries for a run.
func (m *memStore) SaveHostSummary(_ context.Context, runID string, summaries []HostSummary) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(summaries) == 0 {
		delete(m.summaries, runID)
		return nil
	}
	cp := make([]HostSummary, len(summaries))
	copy(cp, summaries)
	for i := range cp {
		cp[i].RunID = runID
	}
	m.summaries[runID] = cp
	return nil
}

// recentByHost groups all host summaries by host, newest first, trimmed to window per host.
func (m *memStore) recentByHost(window int) map[string][]HostSummary {
	byHost := make(map[string][]HostSummary)
	for _, list := range m.summaries {
		for _, hs := range list {
			byHost[hs.Host] = append(byHost[hs.Host], hs)
		}
	}
	for host, list := range byHost {
		sort.Slice(list, func(i, j int) bool { return list[i].RanAt.After(list[j].RanAt) })
		if len(list) > window {
			byHost[host] = list[:window]
		}
	}
	return byHost
}

// FleetHealth ranks hosts by failures over their most recent window runs, worst first.
func (m *memStore) FleetHealth(_ context.Context, window int) ([]HostHealth, error) {
	if window < 1 {
		window = 1
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	byHost := m.recentByHost(window)
	out := make([]HostHealth, 0, len(byHost))
	for host, recent := range byHost {
		failures := 0
		for _, hs := range recent {
			if FailedOutcome(hs.Worst) {
				failures++
			}
		}
		flips := FlipCount(recent)
		outcomes := make([]string, len(recent))
		for i, hs := range recent {
			outcomes[i] = hs.Worst
		}
		out = append(out, HostHealth{
			Host: host, Failures: failures, Total: len(recent),
			LastOutcome: recent[0].Worst, LastRun: recent[0].RanAt,
			Flips: flips, Flaky: flips >= 2, Recent: outcomes,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Failures != out[j].Failures {
			return out[i].Failures > out[j].Failures
		}
		return out[i].Host < out[j].Host
	})
	return out, nil
}

// HostHistory returns a host's most recent per run summaries, newest first, with run ids.
func (m *memStore) HostHistory(_ context.Context, host string, limit int) ([]HostSummary, error) {
	if limit < 1 {
		limit = 1
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	var out []HostSummary
	for _, list := range m.summaries {
		for _, hs := range list {
			if hs.Host == host {
				out = append(out, hs)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RanAt.After(out[j].RanAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SaveTaskSummary replaces the stored per task summaries for a run.
func (m *memStore) SaveTaskSummary(_ context.Context, runID string, summaries []TaskSummary) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(summaries) == 0 {
		delete(m.tasks, runID)
		return nil
	}
	cp := make([]TaskSummary, len(summaries))
	copy(cp, summaries)
	for i := range cp {
		cp[i].RunID = runID
	}
	m.tasks[runID] = cp
	return nil
}

// TaskTrends aggregates each task's durations over its most recent window runs.
func (m *memStore) TaskTrends(_ context.Context, window int) ([]TaskTrend, error) {
	if window < 1 {
		window = 1
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	byTask := make(map[string][]TaskSummary)
	for _, list := range m.tasks {
		for _, ts := range list {
			byTask[ts.Task] = append(byTask[ts.Task], ts)
		}
	}

	out := make([]TaskTrend, 0, len(byTask))
	for task, list := range byTask {
		sort.Slice(list, func(i, j int) bool { return list[i].RanAt.After(list[j].RanAt) })
		recent := list
		if len(recent) > window {
			recent = recent[:window]
		}
		total := 0.0
		for _, ts := range recent {
			total += ts.Seconds
		}
		out = append(out, TaskTrend{
			Task: task, Runs: len(recent), AvgSeconds: total / float64(len(recent)),
			LastSeconds: recent[0].Seconds, LastRun: recent[0].RanAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Task < out[j].Task })
	return out, nil
}

// HostCosts returns each host's average recorded duration in seconds over its most recent window
// runs, for balancing splits by past cost.
func (m *memStore) HostCosts(_ context.Context, window int) (map[string]float64, error) {
	if window < 1 {
		window = 1
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	byHost := make(map[string][]HostSummary)
	for _, list := range m.summaries {
		for _, hs := range list {
			byHost[hs.Host] = append(byHost[hs.Host], hs)
		}
	}

	out := make(map[string]float64, len(byHost))
	for host, list := range byHost {
		sort.Slice(list, func(i, j int) bool { return list[i].RanAt.After(list[j].RanAt) })
		recent := list
		if len(recent) > window {
			recent = recent[:window]
		}
		total := 0.0
		for _, hs := range recent {
			total += hs.DurationSeconds
		}
		out[host] = total / float64(len(recent))
	}
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
