package run

import (
	"context"
	"maps"
	"sort"
	"time"
)

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
		switch r.Status {
		case StatusRunning:
			w.Active++
		case StatusSucceeded:
			w.Completed++
		case StatusFailed:
			w.Failed++
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

// RunHostSummaries returns one run's stored per host summaries, ordered by host.
func (m *memStore) RunHostSummaries(_ context.Context, runID string) ([]HostSummary, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]HostSummary, len(m.summaries[runID]))
	copy(out, m.summaries[runID])
	for i := range out {
		out[i].RunID = runID
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

// SaveHostSummary replaces the stored per host summaries for a run, stamping each row with the
// run's id and its dry-run flag so later reads need nothing from the run record itself.
func (m *memStore) SaveHostSummary(_ context.Context, runID string, summaries []HostSummary) error {
	// Cleaned here so every backend stores the same bytes for the same input, the way Save does for
	// the run itself.
	SanitizeHostSummaries(summaries)
	m.mu.Lock()
	defer m.mu.Unlock()
	r, known := m.runs[runID]
	if known && r.Status.Terminal() {
		// Fence a terminal run so a reclaimed-but-alive worker cannot overwrite the final summary.
		return nil
	}
	if len(summaries) == 0 {
		delete(m.summaries, runID)
		return nil
	}
	dry := known && r.DryRun
	cp := make([]HostSummary, len(summaries))
	copy(cp, summaries)
	for i := range cp {
		cp[i].RunID = runID
		cp[i].DryRun = dry
	}
	m.summaries[runID] = cp
	return nil
}

// AppendHostSummary upserts the given per-host summaries into the run's set, keyed by host, leaving
// hosts already recorded in place. It fences a terminal run and ignores an empty batch, matching
// SaveHostSummary, so a relay report continued across batches accumulates the same way it would in a
// persisting store.
func (m *memStore) AppendHostSummary(_ context.Context, runID string, summaries []HostSummary) error {
	// Cleaned here so every backend stores the same bytes for the same input, the way Save does for
	// the run itself.
	SanitizeHostSummaries(summaries)
	m.mu.Lock()
	defer m.mu.Unlock()
	r, known := m.runs[runID]
	if known && r.Status.Terminal() {
		return nil
	}
	if len(summaries) == 0 {
		return nil
	}
	dry := known && r.DryRun
	existing := m.summaries[runID]
	at := make(map[string]int, len(existing))
	for i := range existing {
		at[existing[i].Host] = i
	}
	for _, hs := range summaries {
		hs.RunID = runID
		hs.DryRun = dry
		if i, ok := at[hs.Host]; ok {
			existing[i] = hs
		} else {
			at[hs.Host] = len(existing)
			existing = append(existing, hs)
		}
	}
	m.summaries[runID] = existing
	return nil
}

// newerHostSummary reports whether a comes before b when host summaries read newest first. Two
// summaries can share an instant, and the map they are gathered from has no order, so the run id
// decides ties, descending. Without it the answer changes from one call to the next and disagrees
// with the SQL stores, which break the same tie the same way.
func newerHostSummary(a, b HostSummary) bool {
	if !a.RanAt.Equal(b.RanAt) {
		return a.RanAt.After(b.RanAt)
	}
	return a.RunID > b.RunID
}

// newerTaskSummary reports whether a comes before b when task summaries read newest first, breaking
// an equal instant by run id, descending, for the same reason as newerHostSummary.
func newerTaskSummary(a, b TaskSummary) bool {
	if !a.RanAt.Equal(b.RanAt) {
		return a.RanAt.After(b.RanAt)
	}
	return a.RunID > b.RunID
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
		sort.Slice(list, func(i, j int) bool { return newerHostSummary(list[i], list[j]) })
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
		runIDs := make([]string, len(recent))
		for i, hs := range recent {
			outcomes[i] = hs.Worst
			runIDs[i] = hs.RunID
		}
		out = append(out, HostHealth{
			Host: host, Failures: failures, Total: len(recent),
			LastOutcome: recent[0].Worst, LastRun: recent[0].RanAt,
			Flips: flips, Flaky: flips >= 2, Recent: outcomes, RecentRuns: runIDs,
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
// drift first. A host with no dry run in its history is omitted, having no drift signal. It reads
// the dry-run flag stamped on the summary rather than the run record, which retention deletes, so
// it reports on exactly the history fleet health reports on.
func (m *memStore) DriftStatus(_ context.Context) ([]HostDrift, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	latest := make(map[string]HostSummary)
	for _, summaries := range m.summaries {
		for _, hs := range summaries {
			if !hs.DryRun {
				continue
			}
			if cur, seen := latest[hs.Host]; !seen || newerHostSummary(hs, cur) {
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

// SaveHostFacts records each host's gathered facts, replacing whatever was held before, since the
// newest gather is the truth about a host.
func (m *memStore) SaveHostFacts(_ context.Context, runID string, facts []HostFacts) error {
	// Cleaned here so every backend stores the same bytes for the same input, the way Save does for
	// the run itself.
	SanitizeHostFacts(facts)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.facts == nil {
		m.facts = make(map[string]HostFacts)
	}
	for _, f := range facts {
		if f.Host == "" || len(f.Facts) == 0 {
			continue
		}
		cp := f
		cp.RunID = runID
		cp.Facts = maps.Clone(f.Facts)
		if cp.GatheredAt.IsZero() {
			cp.GatheredAt = time.Now()
		}
		m.facts[f.Host] = cp
	}
	return nil
}

// HostFactsFor returns a host's stored facts, or ErrNotFound when it has never been gathered.
func (m *memStore) HostFactsFor(_ context.Context, host string) (*HostFacts, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	f, ok := m.facts[host]
	if !ok {
		return nil, ErrNotFound
	}
	cp := f
	cp.Facts = maps.Clone(f.Facts)
	return &cp, nil
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
	sort.Slice(out, func(i, j int) bool { return newerHostSummary(out[i], out[j]) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// RunTaskSummaries returns one run's stored per task summaries, ordered by task.
func (m *memStore) RunTaskSummaries(_ context.Context, runID string) ([]TaskSummary, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]TaskSummary, len(m.tasks[runID]))
	copy(out, m.tasks[runID])
	for i := range out {
		out[i].RunID = runID
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Task < out[j].Task })
	return out, nil
}

// SaveTaskSummary replaces the stored per task summaries for a run.
func (m *memStore) SaveTaskSummary(_ context.Context, runID string, summaries []TaskSummary) error {
	// Cleaned here so every backend stores the same bytes for the same input, the way Save does for
	// the run itself.
	SanitizeTaskSummaries(summaries)
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

// AppendTaskSummary upserts the given per-task summaries into the run's set, keyed by task, leaving
// tasks already recorded in place, with the same fencing and empty-batch behavior as SaveTaskSummary.
func (m *memStore) AppendTaskSummary(_ context.Context, runID string, summaries []TaskSummary) error {
	// Cleaned here so every backend stores the same bytes for the same input, the way Save does for
	// the run itself.
	SanitizeTaskSummaries(summaries)
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.runs[runID]; ok && r.Status.Terminal() {
		return nil
	}
	if len(summaries) == 0 {
		return nil
	}
	existing := m.tasks[runID]
	at := make(map[string]int, len(existing))
	for i := range existing {
		at[existing[i].Task] = i
	}
	for _, ts := range summaries {
		ts.RunID = runID
		if i, ok := at[ts.Task]; ok {
			existing[i] = ts
		} else {
			at[ts.Task] = len(existing)
			existing = append(existing, ts)
		}
	}
	m.tasks[runID] = existing
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
		sort.Slice(list, func(i, j int) bool { return newerTaskSummary(list[i], list[j]) })
		recent := list
		if len(recent) > window {
			recent = recent[:window]
		}
		total := 0.0
		series := make([]float64, len(recent))
		for i, ts := range recent {
			total += ts.Seconds
			// recent is newest first; the series reads oldest first.
			series[len(recent)-1-i] = ts.Seconds
		}
		out = append(out, TaskTrend{
			Task: task, Runs: len(recent), AvgSeconds: total / float64(len(recent)),
			LastSeconds: recent[0].Seconds, LastRun: recent[0].RanAt, Recent: series,
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
		sort.Slice(list, func(i, j int) bool { return newerHostSummary(list[i], list[j]) })
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

// TrimSummaries keeps the newest keep summaries for each host and each task and drops the rest.
func (m *memStore) TrimSummaries(_ context.Context, keep int) (int, error) {
	if keep < 1 {
		keep = 1
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	deleted := trimNewestPerKey(m.summaries, keep,
		func(hs HostSummary) string { return hs.Host }, newerHostSummary)
	deleted += trimNewestPerKey(m.tasks, keep,
		func(ts TaskSummary) string { return ts.Task }, newerTaskSummary)
	return deleted, nil
}

// summaryRef locates one summary inside the per run slices trimNewestPerKey walks.
type summaryRef struct {
	// runID is the run whose slice holds the summary.
	runID string
	// index is the summary's position in that slice.
	index int
}

// trimNewestPerKey keeps the newest keep entries under each group key across every run's slice in
// byRun and removes the rest, returning how many entries it removed. key groups an entry, and
// newer reports whether a sorts ahead of b when the group is ordered newest first.
func trimNewestPerKey[T any](byRun map[string][]T, keep int, key func(T) string,
	newer func(a, b T) bool) int {
	groups := make(map[string][]summaryRef)
	for runID, list := range byRun {
		for i, entry := range list {
			k := key(entry)
			groups[k] = append(groups[k], summaryRef{runID: runID, index: i})
		}
	}
	drop := make(map[string]map[int]bool)
	removed := 0
	for _, refs := range groups {
		if len(refs) <= keep {
			continue
		}
		sort.Slice(refs, func(i, j int) bool {
			return newer(byRun[refs[i].runID][refs[i].index], byRun[refs[j].runID][refs[j].index])
		})
		for _, ref := range refs[keep:] {
			if drop[ref.runID] == nil {
				drop[ref.runID] = make(map[int]bool)
			}
			drop[ref.runID][ref.index] = true
			removed++
		}
	}
	for runID, indexes := range drop {
		list := byRun[runID]
		kept := make([]T, 0, len(list)-len(indexes))
		for i, entry := range list {
			if !indexes[i] {
				kept = append(kept, entry)
			}
		}
		if len(kept) == 0 {
			delete(byRun, runID)
			continue
		}
		byRun[runID] = kept
	}
	return removed
}
