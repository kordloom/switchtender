package run

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kordloom/switchtender/internal/event"
)

// Store persists runs, their captured log output, and their structured events.
// Implementations must be safe for concurrent use.
type Store interface {
	// Save inserts or replaces the run identified by r.ID. When r carries a non-empty
	// IdempotencyKey that a different run already holds, it makes no change and returns
	// ErrDuplicateKey, the race backstop that lets one of two concurrent submissions win the key.
	// A stored cancel request is sticky: replacing a run whose cancel flag is set keeps the flag
	// set, so saving a stale snapshot cannot erase a cancel another process just requested.
	Save(ctx context.Context, r *Run) error
	// Get returns the run with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*Run, error)
	// ByIdempotencyKey returns the run that holds key, or ErrNotFound when no run does. An empty key
	// is never found, so a keyless submission is never deduped.
	ByIdempotencyKey(ctx context.Context, key string) (*Run, error)
	// List returns top-level runs, excluding shard runs, ordered by creation time, newest first.
	List(ctx context.Context) ([]*Run, error)
	// ListPage returns top-level runs matching filter, capped at limit and skipping offset, so the
	// runs view loads a page at a time. A limit of zero or less returns all of them.
	ListPage(ctx context.Context, filter ListFilter, limit, offset int) ([]*Run, error)
	// RunStatusCounts returns the number of top-level runs in each status. The runs view uses it
	// for the summary cards without loading every run.
	RunStatusCounts(ctx context.Context) (map[Status]int, error)
	// Shards returns the shard runs of a parent ordered by shard index.
	Shards(ctx context.Context, parentID string) ([]*Run, error)
	// Steps returns the pipeline step runs of a parent ordered by step index.
	Steps(ctx context.Context, parentID string) ([]*Run, error)
	// NonTerminal returns all runs, including shards, that are not in a terminal state.
	NonTerminal(ctx context.Context) ([]*Run, error)
	// Claim leases the oldest unclaimed pending executable run whose queue this owner serves and
	// returns it, or ErrNonePending when nothing is waiting. queues is the set of queue names the
	// caller serves; a run with an empty queue is on the default pool. Plain runs, shard
	// children, and pipeline step children are executable; split and pipeline parents are
	// coordination records and are not. The claim must be atomic across processes.
	Claim(ctx context.Context, owner string, queues []string) (*Run, error)
	// Heartbeat renews owner's lease on a run. It returns ErrNotFound when the run is gone or the
	// lease is no longer held by owner.
	Heartbeat(ctx context.Context, id, owner string) error
	// ReclaimStale requeues pending runs whose lease renewal is older than cutoff and marks stale
	// running runs interrupted, returning how many rows changed. It sweeps up after dead workers.
	ReclaimStale(ctx context.Context, cutoff time.Time) (int, error)
	// RequestCancel marks the run so whichever process holds it stops it, or ErrNotFound.
	RequestCancel(ctx context.Context, id string) error
	// CancelPending atomically cancels a run that is waiting unclaimed in pending or
	// pending_approval and reports whether it changed the run. It reports false for a missing,
	// claimed, executing, or terminal run; those are canceled cooperatively through RequestCancel
	// by whichever process holds them.
	CancelPending(ctx context.Context, id string) (bool, error)
	// TransitionStatus atomically moves the run from the from status to the to status and reports
	// whether it changed a row. It changes nothing and returns false when the run is missing or is
	// not in the from status, so two callers racing to approve or reject the same run cannot both win.
	TransitionStatus(ctx context.Context, id string, from, to Status) (bool, error)
	// Workers lists executors by the leases they hold, most recently seen first. Only leases
	// stamped within WorkerWindow count, so the listing stays bounded as run history grows.
	Workers(ctx context.Context) ([]WorkerInfo, error)
	// SaveHostSummary replaces the stored per host summaries for a run.
	SaveHostSummary(ctx context.Context, runID string, summaries []HostSummary) error
	// FleetHealth ranks hosts by failures over their most recent window runs, worst first.
	FleetHealth(ctx context.Context, window int) ([]HostHealth, error)
	// DriftStatus reports each host's most recent drift check, the latest dry run to touch it, worst
	// drift first. A host with no dry run in its history is omitted, having no drift signal.
	DriftStatus(ctx context.Context) ([]HostDrift, error)
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
	// LogAfter returns the run's log chunks whose store sequence is greater than afterSeq, in
	// order, capped at limit chunks. A limit of zero or less returns every matching chunk. Each
	// chunk carries its Seq, so a caller streams new output by passing the last Seq it saw back
	// as afterSeq. Seq values are opaque and monotonic within a run. Returns ErrNotFound if the
	// run is absent.
	LogAfter(ctx context.Context, id string, afterSeq int64, limit int) ([]LogChunk, error)
	// LastLogSeq returns the store sequence of the run's most recent log chunk, or zero when the
	// run has no log. A live stream starts from it to send only what lands next without reading
	// the output already stored. Returns ErrNotFound if the run is absent.
	LastLogSeq(ctx context.Context, id string) (int64, error)
	// AppendEvents appends structured events to the run. Returns ErrNotFound if the run is absent.
	AppendEvents(ctx context.Context, id string, events []event.Event) error
	// Events returns a copy of the run's structured events, or ErrNotFound.
	Events(ctx context.Context, id string) ([]event.Event, error)
	// EventsAfter returns the run's events whose store sequence is greater than afterSeq, in
	// order, capped at limit. A limit of zero or less returns every matching event. Each event
	// carries its Seq, so a caller pages or streams by passing the last Seq it saw back as
	// afterSeq. Returns ErrNotFound if the run is absent.
	EventsAfter(ctx context.Context, id string, afterSeq int64, limit int) ([]event.Event, error)
	// LastEventSeq returns the store sequence of the run's most recent event, or zero when the
	// run has no events. A live stream starts from it to send only what lands next without
	// reading the existing log. Returns ErrNotFound if the run is absent.
	LastEventSeq(ctx context.Context, id string) (int64, error)
	// PurgeEventsBefore drops the events and logs of terminal runs created before cutoff, keeping
	// the run records and their summaries. It returns how many runs were trimmed.
	PurgeEventsBefore(ctx context.Context, cutoff time.Time) (int, error)
	// PurgeRunsBefore deletes terminal runs created before cutoff along with their events and logs,
	// keeping the per host and per task summaries that power the cross-run views. It returns how
	// many runs were deleted. Non-terminal runs are never purged.
	PurgeRunsBefore(ctx context.Context, cutoff time.Time) (int, error)
}

// WorkerWindow bounds how far back Workers looks for leases. Terminal runs keep their last lease
// stamp, so without a bound the listing would aggregate every run ever recorded and report
// workers dead for months.
const WorkerWindow = 48 * time.Hour

// LogChunk is one stored piece of a run's log. Seq orders chunks within the run and serves as an
// opaque cursor for LogAfter.
type LogChunk struct {
	// Seq is the chunk's store sequence, monotonic within the run.
	Seq int64
	// Data is the chunk's raw bytes.
	Data []byte
}

// ListFilter narrows and orders a runs listing. Zero values apply no constraint, so the empty
// filter lists everything newest first.
type ListFilter struct {
	// Query is a free-text term matched case-insensitively across the fields the runs view shows.
	Query string
	// Status keeps only runs with exactly this status when set.
	Status string
	// Tool keeps only runs of this normalized tool when set. Ansible matches runs stored with an
	// empty tool, its historical form.
	Tool string
	// OldestFirst flips the newest-first default ordering.
	OldestFirst bool
}

// memStore is an in-memory Store backed by maps guarded by a read-write mutex.
type memStore struct {
	// mu guards runs, logs, and events.
	mu sync.RWMutex
	// runs maps run id to the stored run.
	runs map[string]*Run
	// byKey maps a non-empty idempotency key to the id of the run that holds it, mirroring the
	// partial unique index the SQL backends use to dedupe submissions.
	byKey map[string]string
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
		byKey:     make(map[string]string),
		logs:      make(map[string][]byte),
		events:    make(map[string][]event.Event),
		summaries: make(map[string][]HostSummary),
		tasks:     make(map[string][]TaskSummary),
	}
}

// Save inserts or replaces the run identified by r.ID. A non-empty idempotency key already held by a
// different run is rejected with ErrDuplicateKey so a concurrent retry cannot create a second run. A
// stored cancel request survives the replace so a stale snapshot cannot erase a concurrent cancel.
func (m *memStore) Save(_ context.Context, r *Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.IdempotencyKey != "" {
		if owner, ok := m.byKey[r.IdempotencyKey]; ok && owner != r.ID {
			return ErrDuplicateKey
		}
	}
	cl := r.Clone()
	if prev, ok := m.runs[r.ID]; ok && prev.CancelRequested {
		cl.CancelRequested = true
	}
	m.runs[r.ID] = cl
	if _, ok := m.logs[r.ID]; !ok {
		m.logs[r.ID] = nil
	}
	if r.IdempotencyKey != "" {
		m.byKey[r.IdempotencyKey] = r.ID
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

// ByIdempotencyKey returns the run that holds key, or ErrNotFound. An empty key is never found.
func (m *memStore) ByIdempotencyKey(_ context.Context, key string) (*Run, error) {
	if key == "" {
		return nil, ErrNotFound
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.byKey[key]
	if !ok {
		return nil, ErrNotFound
	}
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

// ListPage returns a page of top-level runs newest first, capped at limit and skipping offset,
// optionally filtered by a case-insensitive search term.
func (m *memStore) ListPage(ctx context.Context, filter ListFilter, limit, offset int) ([]*Run, error) {
	all, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	term := strings.ToLower(strings.TrimSpace(filter.Query))
	matched := make([]*Run, 0, len(all))
	for _, r := range all {
		if term != "" && !matchesQuery(r, term) {
			continue
		}
		if filter.Status != "" && string(r.Status) != filter.Status {
			continue
		}
		if filter.Tool != "" && NormalizeTool(r.Tool) != filter.Tool {
			continue
		}
		matched = append(matched, r)
	}
	all = matched
	if filter.OldestFirst {
		slices.Reverse(all)
	}
	if offset >= len(all) {
		return []*Run{}, nil
	}
	all = all[offset:]
	if limit > 0 && limit < len(all) {
		all = all[:limit]
	}
	return all, nil
}

// matchesQuery reports whether the run matches a lowercased search term across the fields the runs
// view shows: id, playbook, command, tool, status, step name, and inventory.
func matchesQuery(r *Run, term string) bool {
	for _, field := range []string{r.ID, r.Playbook, r.Command, r.Tool, string(r.Status), r.StepName, r.Inventory} {
		if strings.Contains(strings.ToLower(field), term) {
			return true
		}
	}
	return false
}

// RunStatusCounts tallies top-level runs by status.
func (m *memStore) RunStatusCounts(_ context.Context) (map[Status]int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[Status]int{}
	for _, r := range m.runs {
		if r.ParentID != nil {
			continue
		}
		out[r.Status]++
	}
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
func (m *memStore) Claim(_ context.Context, owner string, queues []string) (*Run, error) {
	serves := make(map[string]bool, len(queues))
	for _, q := range queues {
		serves[q] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var oldest *Run
	for _, r := range m.runs {
		if r.Status != StatusPending || r.ClaimedBy != "" || r.Kind != "" || r.CancelRequested {
			continue
		}
		if !serves[r.Queue] {
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
			r.ClaimedBy = ""
			r.ClaimedAt = nil
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

// CancelPending atomically cancels a run still waiting unclaimed in pending or pending_approval.
func (m *memStore) CancelPending(_ context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok || r.ClaimedBy != "" {
		return false, nil
	}
	if r.Status != StatusPending && r.Status != StatusPendingApproval {
		return false, nil
	}
	now := time.Now()
	r.Status = StatusCanceled
	r.EndedAt = &now
	return true, nil
}

// TransitionStatus atomically moves the run from one status to another, reporting whether it changed.
func (m *memStore) TransitionStatus(_ context.Context, id string, from, to Status) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok || r.Status != from {
		return false, nil
	}
	r.Status = to
	return true, nil
}

// Workers lists executors by the leases they hold within WorkerWindow, most recently seen first.
func (m *memStore) Workers(_ context.Context) ([]WorkerInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cutoff := time.Now().Add(-WorkerWindow)
	byOwner := make(map[string]*WorkerInfo)
	for _, r := range m.runs {
		if r.ClaimedBy == "" || r.ClaimedAt == nil || r.ClaimedAt.Before(cutoff) {
			continue
		}
		w, ok := byOwner[r.ClaimedBy]
		if !ok {
			w = &WorkerInfo{Owner: r.ClaimedBy}
			byOwner[r.ClaimedBy] = w
		}
		if r.Status == StatusRunning {
			w.Active++
		}
		if r.ClaimedAt.After(w.LastSeen) {
			w.LastSeen = *r.ClaimedAt
		}
	}
	out := make([]WorkerInfo, 0, len(byOwner))
	for _, w := range byOwner {
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].Owner < out[j].Owner
		}
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	return out, nil
}

// SaveHostSummary replaces the stored per host summaries for a run.
func (m *memStore) SaveHostSummary(_ context.Context, runID string, summaries []HostSummary) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.runs[runID]; ok && r.Status.Terminal() {
		// Fence a terminal run so a reclaimed-but-alive worker cannot overwrite the final summary.
		return nil
	}
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

// DriftStatus reports each host's most recent drift check, the latest dry run to touch it, worst
// drift first. A host with no dry run in its history is omitted, having no drift signal.
func (m *memStore) DriftStatus(_ context.Context) ([]HostDrift, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	latest := make(map[string]HostSummary)
	for runID, summaries := range m.summaries {
		r, ok := m.runs[runID]
		if !ok || !r.DryRun {
			continue
		}
		for _, hs := range summaries {
			if cur, seen := latest[hs.Host]; !seen || hs.RanAt.After(cur.RanAt) {
				latest[hs.Host] = hs
			}
		}
	}

	out := make([]HostDrift, 0, len(latest))
	for host, hs := range latest {
		out = append(out, HostDrift{
			Host: host, DriftedTasks: hs.Changed, RunID: hs.RunID, CheckedAt: hs.RanAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DriftedTasks != out[j].DriftedTasks {
			return out[i].DriftedTasks > out[j].DriftedTasks
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
	if r, ok := m.runs[runID]; ok && r.Status.Terminal() {
		// Fence a terminal run so a reclaimed-but-alive worker cannot overwrite the final summary.
		return nil
	}
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
	r, ok := m.runs[id]
	if !ok {
		return ErrNotFound
	}
	if r.Status.Terminal() {
		// Fence a terminal run so a reclaimed-but-alive worker cannot append to a run that has ended.
		return nil
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

// LogAfter returns the log bytes past afterSeq as a single chunk. The memory store's log sequence
// is the byte offset, so the returned chunk carries the total length as its Seq.
func (m *memStore) LogAfter(_ context.Context, id string, afterSeq int64, _ int) ([]LogChunk, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.runs[id]; !ok {
		return nil, ErrNotFound
	}
	buf := m.logs[id]
	if afterSeq < 0 {
		afterSeq = 0
	}
	if afterSeq >= int64(len(buf)) {
		return nil, nil
	}
	out := make([]byte, int64(len(buf))-afterSeq)
	copy(out, buf[afterSeq:])
	return []LogChunk{{Seq: int64(len(buf)), Data: out}}, nil
}

// LastLogSeq returns the byte length of the run's log, the memory store's log sequence.
func (m *memStore) LastLogSeq(_ context.Context, id string) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.runs[id]; !ok {
		return 0, ErrNotFound
	}
	return int64(len(m.logs[id])), nil
}

// AppendEvents appends structured events to the run. Returns ErrNotFound if the run is absent.
func (m *memStore) AppendEvents(_ context.Context, id string, events []event.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return ErrNotFound
	}
	if r.Status.Terminal() {
		// Fence a terminal run so a reclaimed-but-alive worker cannot stream events into it.
		return nil
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

// EventsAfter returns the run's events past afterSeq, capped at limit. The sequence is the
// one-based position, so it is monotonic within a run and usable as an opaque paging cursor.
func (m *memStore) EventsAfter(_ context.Context, id string, afterSeq int64, limit int) ([]event.Event, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.runs[id]; !ok {
		return nil, ErrNotFound
	}
	var out []event.Event
	for i, e := range m.events[id] {
		seq := int64(i + 1)
		if seq <= afterSeq {
			continue
		}
		e.Seq = seq
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// LastEventSeq returns the one-based position of the run's last event, or zero when it has none.
func (m *memStore) LastEventSeq(_ context.Context, id string) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.runs[id]; !ok {
		return 0, ErrNotFound
	}
	return int64(len(m.events[id])), nil
}

// PurgeEventsBefore drops the events and logs of terminal runs created before cutoff, keeping the
// run records and their summaries. It returns how many runs were trimmed.
func (m *memStore) PurgeEventsBefore(_ context.Context, cutoff time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	trimmed := 0
	for id, r := range m.runs {
		if !r.Status.Terminal() || !r.CreatedAt.Before(cutoff) {
			continue
		}
		if len(m.events[id]) == 0 && len(m.logs[id]) == 0 {
			continue
		}
		delete(m.events, id)
		delete(m.logs, id)
		trimmed++
	}
	return trimmed, nil
}

// PurgeRunsBefore deletes terminal runs created before cutoff along with their events and logs,
// keeping the per host and per task summaries. It returns how many runs were deleted.
func (m *memStore) PurgeRunsBefore(_ context.Context, cutoff time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	deleted := 0
	for id, r := range m.runs {
		if !r.Status.Terminal() || !r.CreatedAt.Before(cutoff) {
			continue
		}
		if r.IdempotencyKey != "" && m.byKey[r.IdempotencyKey] == id {
			delete(m.byKey, r.IdempotencyKey)
		}
		delete(m.runs, id)
		delete(m.events, id)
		delete(m.logs, id)
		deleted++
	}
	return deleted, nil
}
