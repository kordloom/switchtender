package run

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kordloom/switchtender/internal/event"
	"slices"
)

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
	// facts holds the most recently gathered system facts per host.
	facts map[string]HostFacts
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
	// Cleaned here so every backend stores the same bytes for the same input.
	r.Sanitize()
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
		if !filter.After.IsZero() && r.CreatedAt.Before(filter.After) {
			continue
		}
		if !filter.Before.IsZero() && !r.CreatedAt.Before(filter.Before) {
			continue
		}
		if filter.Source != "" && r.Source != filter.Source {
			continue
		}
		if filter.Actor != "" && r.Actor != filter.Actor {
			continue
		}
		if filter.SourceID != "" && r.SourceID != filter.SourceID {
			continue
		}
		// The key has to be present. Comparing the map lookup directly matched a run carrying no
		// such label against an empty value, which neither SQL store does.
		if filter.LabelKey != "" {
			got, ok := r.Labels[filter.LabelKey]
			if !ok || got != filter.LabelValue {
				continue
			}
		}
		if filter.ClaimedBy != "" && r.ClaimedBy != filter.ClaimedBy {
			continue
		}
		if filter.HeldBy != "" && r.HeldByPolicy != filter.HeldBy {
			continue
		}
		if filter.Host != "" && !m.runTouchedHost(r.ID, filter.Host) {
			continue
		}
		matched = append(matched, r)
	}
	all = matched
	if filter.OldestFirst {
		slices.Reverse(all)
	}
	// Offset applies only alongside a limit, which is what both SQL stores do: their OFFSET clause
	// is emitted inside the same "if limit > 0" branch as LIMIT. Applying it unconditionally here
	// made an unlimited page skip rows on this store and return everything on the other two, and
	// every dispatch test runs against this one.
	if limit > 0 {
		if offset >= len(all) {
			return []*Run{}, nil
		}
		all = all[offset:]
		if limit < len(all) {
			all = all[:limit]
		}
	}
	return all, nil
}

// runTouchedHost reports whether the run's stored host summaries include the host.
//
// It takes the read lock itself. The comment here used to say the caller held it through ListPage's
// call into List, and that was simply untrue: List takes the lock and releases it before returning,
// so ListPage held nothing while reading the summaries map. A concurrent SaveHostSummary made that a
// concurrent map read and write, which is a fatal runtime error rather than a detector warning.
func (m *memStore) runTouchedHost(runID, host string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, hs := range m.summaries[runID] {
		if hs.Host == host {
			return true
		}
	}
	return false
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

// RunTimings returns the timing fields of the most recent top-level runs, newest first.
func (m *memStore) RunTimings(_ context.Context, limit int) ([]RunTiming, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Run, 0, len(m.runs))
	for _, r := range m.runs {
		if r.ParentID == nil {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	timings := make([]RunTiming, 0, len(out))
	for _, r := range out {
		timings = append(timings, RunTiming{
			ID: r.ID, Status: r.Status, Kind: r.Kind, Queue: r.Queue, ClaimedBy: r.ClaimedBy,
			CreatedAt: r.CreatedAt, StartedAt: r.StartedAt, EndedAt: r.EndedAt,
		})
	}
	return timings, nil
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
	// A child with no shard index sorts last. Indexed shards are the ordered fan-out and an
	// unindexed child is the exception, so it goes after them. The SQL stores say so explicitly
	// rather than relying on a default, because SQLite orders nulls first and Postgres orders them
	// last, and the three implementations disagreed for exactly that reason.
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
//
// A child is claimable only while its parent is running. Shards are stored before the coordinator
// fences the parent, so for as long as that parent is merely pending its shards are already sitting
// claimable: a split canceled in that window had the fence correctly refuse to start the parent
// while a claim loop had already taken shards and executed them on real hosts. Allowing a pending
// parent narrowed that window rather than closing it, and under load the loop still won.
//
// Running is the state that says a coordinator took the parent and means to run it, and every path
// that creates a claimable child reaches it: a split and a shard retry both transition the parent
// through the start fence, and pipeline steps are created only after it. A parent whose coordinator
// dies before the fence leaves its children unclaimable, which the abandoned-parent sweep settles.
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
		if r.ParentID != nil {
			p, ok := m.runs[*r.ParentID]
			if !ok || p.CancelRequested || p.Status != StatusRunning {
				continue
			}
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
	// Mint a fresh capability for this claim. The worker receives it once, over the relay, and
	// presents it on every report it makes. A later reclaim replaces it, so a report a worker
	// minted against a claim it has since lost no longer verifies.
	oldest.ClaimSecret = NewClaimSecret()
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

// ReclaimStale requeues stale claimed pending runs, interrupts stale running runs, and interrupts
// split or pipeline parents left pending with no coordinator. The lease was stamped by this same
// process, so its own clock is the authoritative one. Interrupting a parent orphans its children, so
// they are resolved in the same sweep. See AbandonedParent for what makes a parent unrecoverable.
// ReclaimStaleSettled sweeps like ReclaimStale and names the top-level runs this sweep itself drove
// terminal, so a caller can record their outcomes. A child's outcome rolls up into its parent, so
// children are left out the same way the terminal save leaves them out. Attribution comes from the
// sweep's own writes, under the same lock: a before-and-after diff credited the sweep with any run
// that happened to finish while it ran, and the caller then committed a second outcome for a run
// whose real finisher had already committed one.
func (m *memStore) ReclaimStaleSettled(ctx context.Context, ttl time.Duration) (int, []string, error) {
	n, settled, err := m.reclaimStale(ctx, ttl)
	sort.Strings(settled)
	return n, settled, err
}

func (m *memStore) ReclaimStale(ctx context.Context, ttl time.Duration) (int, error) {
	n, _, err := m.reclaimStale(ctx, ttl)
	return n, err
}

// reclaimStale is the sweep, returning both how many rows it changed and which top-level runs it
// drove terminal. It runs under one lock acquisition, so what it reports is exactly what it did.
func (m *memStore) reclaimStale(_ context.Context, ttl time.Duration) (int, []string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().Add(-ttl)
	changed := 0
	var settled []string
	for id, r := range m.runs {
		// An abandoned parent is interrupted before orphans are resolved, so its children settle in
		// the same sweep rather than waiting for the next one.
		if AbandonedParent(r, cutoff) {
			now := time.Now()
			r.Status = StatusInterrupted
			r.EndedAt = &now
			if r.Error == "" {
				r.Error = abandonedParentError
			}
			changed++
			if r.ParentID == nil {
				settled = append(settled, id)
			}
			continue
		}
		if r.ClaimedBy == "" || r.ClaimedAt == nil || !r.ClaimedAt.Before(cutoff) {
			continue
		}
		switch r.Status {
		case StatusPending:
			r.ClaimedBy = ""
			r.ClaimedAt = nil
			r.ClaimSecret = ""
			// A run somebody asked to cancel is settled rather than put back in the queue. Canceling a
			// claimed run is cooperative, so if its holder died before starting it the flag has nobody
			// left to read it, and a claim will not take a cancel-flagged run: requeuing left it pending
			// and unclaimable with nothing that sweeps a pending run to end it.
			if r.CancelRequested {
				now := time.Now()
				r.Status = StatusCanceled
				r.EndedAt = &now
			}
			changed++
		case StatusRunning:
			now := time.Now()
			r.Status = StatusInterrupted
			r.ClaimedBy = ""
			r.ClaimedAt = nil
			r.ClaimSecret = ""
			r.EndedAt = &now
			if r.Error == "" {
				r.Error = "interrupted: executor lease expired"
			}
			changed++
			if r.ParentID == nil {
				settled = append(settled, id)
			}
		}
	}
	return changed + m.resolveOrphans(), settled, nil
}

// resolveOrphans settles the children of an interrupted parent, whose coordinator died holding the
// rollup. A child no executor has started is canceled outright, since nothing will ever collect it
// and leaving it pending means it is still claimable long after its split is over. A child already
// executing is asked to stop through the cancel flag its executor watches. It runs under m.mu.
func (m *memStore) resolveOrphans() int {
	orphaned := make(map[string]bool)
	for id, r := range m.runs {
		if r.Status == StatusInterrupted && (r.Kind == KindSplit || r.Kind == KindPipeline) {
			orphaned[id] = true
		}
	}
	if len(orphaned) == 0 {
		return 0
	}
	changed := 0
	for _, r := range m.runs {
		if r.ParentID == nil || !orphaned[*r.ParentID] {
			continue
		}
		switch r.Status {
		case StatusPending, StatusPendingApproval:
			now := time.Now()
			r.Status = StatusCanceled
			r.ClaimedBy = ""
			r.ClaimedAt = nil
			r.ClaimSecret = ""
			r.EndedAt = &now
			if r.Error == "" {
				r.Error = orphanError
			}
			changed++
		case StatusRunning:
			if !r.CancelRequested {
				r.CancelRequested = true
				changed++
			}
		}
	}
	return changed
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

// TransitionStatusAndClaim moves the run between statuses and takes owner's lease in one step.
// A requested cancel blocks the claim in the same statement that makes it. Cancel is recorded as a
// flag rather than a status, so a fence that compares only the status cannot see one: a pipeline
// canceled after it was approved and before its coordinator picked it up still read as running, won
// the compare-and-swap, and executed on real hosts. Checking the flag first and swapping second
// leaves the same gap one scheduling delay wide, so it belongs in the predicate.
func (m *memStore) TransitionStatusAndClaim(_ context.Context, id string, from, to Status,
	owner string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok || r.Status != from || r.CancelRequested {
		return false, nil
	}
	now := time.Now()
	r.Status = to
	r.ClaimedBy = owner
	r.ClaimedAt = &now
	if r.StartedAt == nil {
		r.StartedAt = &now
	}
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

// StampApprovedSpec records the spec digest an approver decided on, touching nothing else.
func (m *memStore) StampApprovedSpec(_ context.Context, id, digest string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return ErrNotFound
	}
	r.ApprovedSpecDigest = digest
	return nil
}

// FinalizeRunning moves a running run to its terminal status and records the exit code, failure
// detail, resolved image, and end time in the same locked write.
func (m *memStore) FinalizeRunning(_ context.Context, id string, fin Finalization) (bool, error) {
	fin.SanitizeText()
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok || r.Status != StatusRunning {
		return false, nil
	}
	if fin.Owner != "" && r.ClaimedBy != fin.Owner {
		return false, nil
	}
	ended := fin.EndedAt
	r.Status = fin.Status
	r.ExitCode = fin.ExitCode
	r.Error = fin.Error
	r.Image = fin.Image
	r.CommitSHA = fin.CommitSHA
	r.PullCredentialID = fin.PullCredentialID
	r.Outputs = fin.Outputs
	r.Warning = fin.Warning
	r.EndedAt = &ended
	return true, nil
}

// ApplyRunningProgress records a worker's progress on a run it still holds, in one locked write.
func (m *memStore) ApplyRunningProgress(_ context.Context, id, owner string, p Progress) (bool, error) {
	p.SanitizeText()
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok || r.Status != StatusRunning || r.ClaimedBy != owner {
		return false, nil
	}
	if r.StartedAt == nil && p.StartedAt != nil {
		started := *p.StartedAt
		r.StartedAt = &started
	}
	if p.Warning != "" {
		r.Warning = p.Warning
	}
	if len(p.Outputs) > 0 {
		r.Outputs = p.Outputs
	}
	return true, nil
}
