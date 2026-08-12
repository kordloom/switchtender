package run

import (
	"sort"
	"time"

	"github.com/kordloom/switchtender/internal/event"
)

// HostSummary is a single run's outcome for one host, derived from the run recap. It is persisted so
// cross run questions can be answered without reparsing every event.
type HostSummary struct {
	// RunID is the run the summary belongs to, stamped by the store.
	RunID string `json:"run_id,omitempty"`
	// Host is the target host.
	Host string `json:"host"`
	// OK is the count of successful tasks.
	OK int `json:"ok"`
	// Changed is the count of tasks that changed state.
	Changed int `json:"changed"`
	// Failures is the count of failed tasks.
	Failures int `json:"failures"`
	// Unreachable is the count of unreachable results.
	Unreachable int `json:"unreachable"`
	// Skipped is the count of skipped tasks.
	Skipped int `json:"skipped"`
	// Worst is the most severe outcome for the host in the run.
	Worst string `json:"worst"`
	// DurationSeconds is the host's estimated busy time in the run, used to balance future splits.
	DurationSeconds float64 `json:"duration_seconds"`
	// RanAt is when the run was created, used to order host history by recency.
	RanAt time.Time `json:"ran_at"`
	// DryRun reports whether the run was a check rather than an apply, stamped by the store from the
	// run itself. The drift view reads it here so a host keeps its drift entry after retention purges
	// the run row, which is the same history fleet health keeps. Whatever a caller sets is replaced.
	DryRun bool `json:"dry_run,omitempty"`
}

// HostFacts is a host's system facts as of the last run that gathered them. The keys are the
// Ansible fact names without their prefix: distribution, kernel, architecture, and so on.
type HostFacts struct {
	// Host is the host the facts describe.
	Host string `json:"host"`
	// Facts maps a fact name to its value.
	Facts map[string]string `json:"facts"`
	// RunID is the run that gathered them.
	RunID string `json:"run_id,omitempty"`
	// GatheredAt is when that run gathered them.
	GatheredAt time.Time `json:"gathered_at"`
}

// HostFactsFromEvents returns the facts gathered per host. It folds the whole list at once; a caller
// finishing a long run should stream through SummaryFold instead of holding every event in memory.
func HostFactsFromEvents(events []event.Event, at time.Time) []HostFacts {
	f := NewSummaryFold(at)
	f.Add(events)
	return f.HostFacts()
}

// HostHealth summarizes a host's recent reliability across the most recent runs it appeared in.
type HostHealth struct {
	// Host is the target host.
	Host string `json:"host"`
	// Failures is the number of runs in the window where the host failed or was unreachable.
	Failures int `json:"failures"`
	// Total is the number of runs in the window.
	Total int `json:"total"`
	// LastOutcome is the host's worst outcome in its most recent run.
	LastOutcome string `json:"last_outcome"`
	// LastRun is when the host most recently ran.
	LastRun time.Time `json:"last_run"`
	// Flips is how many times the host switched between failing and passing across the window,
	// most recent first.
	Flips int `json:"flips"`
	// Flaky reports that the host switched between failing and passing at least twice in the
	// window, so its failures are intermittent rather than a steady break or a single fix.
	Flaky bool `json:"flaky"`
	// Recent lists the host's worst outcome per run across the window, newest first, for
	// sparkline style displays.
	Recent []string `json:"recent,omitempty"`
	// RecentRuns lists the run behind each Recent entry, index for index, so a display can link
	// every outcome to its run.
	RecentRuns []string `json:"recent_runs,omitempty"`
}

// HostDrift is a host's most recent drift check. A drift check is a dry run: in check mode a changed
// result means a task would change, so the host has diverged from the desired state the playbook
// asserts. It lets the fleet see divergence before the next real run.
type HostDrift struct {
	// Host is the target host.
	Host string `json:"host"`
	// DriftedTasks is how many tasks the latest check would change. Zero means the host is in sync.
	DriftedTasks int `json:"drifted_tasks"`
	// RunID is the check run that observed the drift.
	RunID string `json:"run_id"`
	// CheckedAt is when that check run ran.
	CheckedAt time.Time `json:"checked_at"`
}

// TaskSummary is a single run's wall clock cost for one task, persisted at finalize so task trends
// can be answered without reparsing events.
type TaskSummary struct {
	// RunID is the run the summary belongs to, stamped by the store.
	RunID string `json:"run_id,omitempty"`
	// Task is the task name.
	Task string `json:"task"`
	// Seconds is the wall clock time from the task start to its last host result, summed over the
	// task's occurrences in the run.
	Seconds float64 `json:"seconds"`
	// RanAt is when the run was created, used to order task history by recency.
	RanAt time.Time `json:"ran_at"`
}

// WorkerInfo describes one executor seen through the leases it holds.
type WorkerInfo struct {
	// Owner is the executor's lease name, host and pid or a worker's configured name.
	Owner string `json:"owner"`
	// Active is how many runs the executor holds right now.
	Active int `json:"active"`
	// Completed is how many of its runs in the window finished succeeded.
	Completed int `json:"completed"`
	// Failed is how many of its runs in the window finished failed.
	Failed int `json:"failed"`
	// LastSeen is the freshest lease renewal from this executor.
	LastSeen time.Time `json:"last_seen"`
}

// TaskTrend aggregates a task's recent durations so a task that is getting slower stands out.
type TaskTrend struct {
	// Task is the task name.
	Task string `json:"task"`
	// Runs is the number of recent runs the task appeared in.
	Runs int `json:"runs"`
	// AvgSeconds is the average duration across those runs.
	AvgSeconds float64 `json:"avg_seconds"`
	// LastSeconds is the duration in the most recent run.
	LastSeconds float64 `json:"last_seconds"`
	// LastRun is when the task most recently ran.
	LastRun time.Time `json:"last_run"`
	// Recent is the task's duration in each of those runs, oldest first, so a caller can draw the
	// trend rather than infer it from two numbers.
	Recent []float64 `json:"recent,omitempty"`
}

// HostSummariesFromStats returns the per-host rollup from a run's final stats event. It folds the
// whole list at once; a caller finishing a long run should stream through SummaryFold instead.
func HostSummariesFromStats(events []event.Event, ranAt time.Time) []HostSummary {
	f := NewSummaryFold(ranAt)
	f.Add(events)
	return f.HostSummaries()
}

// worstFromStats reduces a host's recap counts to its most severe outcome.
func worstFromStats(s event.HostStats) string {
	switch {
	case s.Failures > 0:
		return "failed"
	case s.Unreachable > 0:
		return "unreachable"
	case s.Changed > 0:
		return "changed"
	case s.OK > 0:
		return "ok"
	default:
		return "skipped"
	}
}

// OutputsFromEvents returns what a run published through its final stats event. It folds the whole
// list at once; a caller finishing a long run should stream through SummaryFold instead.
func OutputsFromEvents(events []event.Event) map[string]any {
	f := NewSummaryFold(time.Time{})
	f.Add(events)
	return f.Outputs()
}

// FailedOutcome reports whether a worst outcome counts as a failure for reliability ranking.
func FailedOutcome(worst string) bool {
	return worst == "failed" || worst == "unreachable"
}

// FlipCount returns how many times the outcomes switch between failing and passing. The summaries
// must already be ordered; the count is direction agnostic.
func FlipCount(summaries []HostSummary) int {
	flips := 0
	for i := 1; i < len(summaries); i++ {
		if FailedOutcome(summaries[i].Worst) != FailedOutcome(summaries[i-1].Worst) {
			flips++
		}
	}
	return flips
}

// TaskSummariesFromEvents returns how long each task took. It folds the whole list at once; a caller
// finishing a long run should stream through SummaryFold instead.
func TaskSummariesFromEvents(events []event.Event, ranAt time.Time) []TaskSummary {
	f := NewSummaryFold(ranAt)
	f.Add(events)
	return f.TaskSummaries()
}

// SummaryFold accumulates a run's summaries while its events stream past, so finishing a run never
// has to hold its whole event list in memory. A long run can carry hundreds of thousands of events;
// unmarshaling all of them at once cost hundreds of megabytes, and several runs completing together
// could exhaust a small control node. The state kept here is proportional to the number of hosts and
// tasks, not to the number of events.
//
// Every fold it replaces reduced over the events in one pass, so the results are identical to
// computing them from the full list. The batch functions are implemented on top of it, which keeps
// the two paths from drifting.
type SummaryFold struct {
	// ranAt stamps every summary the fold produces.
	ranAt time.Time
	// facts holds the most recent gathered facts per host.
	facts map[string]HostFacts
	// stats holds the most recent stats event's per-host counters.
	stats map[string]event.HostStats
	// outputs holds the most recent stats event's published outputs.
	outputs map[string]any
	// hostSeconds accumulates execution time per host.
	hostSeconds map[string]float64
	// taskSeconds accumulates execution time per task.
	taskSeconds map[string]float64
	// hostTaskStart is when the task currently running started, used for per-host timing.
	hostTaskStart time.Time
	// task is the task currently running, and taskStart when it began.
	task      string
	taskStart time.Time
	// lastResult is the latest result seen for the current task.
	lastResult time.Time
}

// NewSummaryFold returns an empty fold that stamps its summaries with ranAt.
func NewSummaryFold(ranAt time.Time) *SummaryFold {
	return &SummaryFold{
		ranAt:       ranAt,
		facts:       make(map[string]HostFacts),
		hostSeconds: make(map[string]float64),
		taskSeconds: make(map[string]float64),
	}
}

// Add folds a batch of events in order. Batches must arrive in the order the run produced them,
// since task timing depends on a start preceding its results.
func (f *SummaryFold) Add(events []event.Event) {
	for _, e := range events {
		switch e.Type {
		case event.TypeFacts:
			if e.Host != "" && len(e.Facts) > 0 {
				f.facts[e.Host] = HostFacts{Host: e.Host, Facts: e.Facts, GatheredAt: f.ranAt}
			}
		case event.TypeStats:
			if e.Stats != nil {
				f.stats = e.Stats
			}
			if len(e.Outputs) > 0 {
				f.outputs = e.Outputs
			}
		case event.TypeTaskStart:
			f.closeTask()
			f.task, f.taskStart, f.lastResult = e.Task, e.Time, time.Time{}
			f.hostTaskStart = e.Time
		case event.TypeRunnerOK, event.TypeRunnerFailed, event.TypeRunnerSkipped,
			event.TypeRunnerUnreachable:
			if e.Time.After(f.lastResult) {
				f.lastResult = e.Time
			}
			if e.Host == "" || f.hostTaskStart.IsZero() {
				continue
			}
			if gap := e.Time.Sub(f.hostTaskStart).Seconds(); gap > 0 {
				f.hostSeconds[e.Host] += gap
			}
		}
	}
}

// closeTask banks the current task's elapsed time. It runs when a new task starts.
func (f *SummaryFold) closeTask() {
	if f.task != "" && f.lastResult.After(f.taskStart) {
		f.taskSeconds[f.task] += f.lastResult.Sub(f.taskStart).Seconds()
	}
}

// HostSummaries returns the per-host rollup from the run's final stats event, or nil when it never
// reported one.
func (f *SummaryFold) HostSummaries() []HostSummary {
	if f.stats == nil {
		return nil
	}
	out := make([]HostSummary, 0, len(f.stats))
	for host, s := range f.stats {
		out = append(out, HostSummary{
			Host: host, OK: s.OK, Changed: s.Changed, Failures: s.Failures,
			Unreachable: s.Unreachable, Skipped: s.Skipped, Worst: worstFromStats(s),
			DurationSeconds: f.hostSeconds[host], RanAt: f.ranAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// HostFacts returns the gathered facts per host, or nil when none were gathered.
func (f *SummaryFold) HostFacts() []HostFacts {
	if len(f.facts) == 0 {
		return nil
	}
	out := make([]HostFacts, 0, len(f.facts))
	for _, hf := range f.facts {
		out = append(out, hf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// TaskSummaries returns the per-task durations, banking the task still open. It does not mutate the
// fold, so calling it more than once returns the same answer and folding may continue afterwards.
func (f *SummaryFold) TaskSummaries() []TaskSummary {
	totals := f.taskSeconds
	if f.task != "" && f.lastResult.After(f.taskStart) {
		totals = make(map[string]float64, len(f.taskSeconds)+1)
		for k, v := range f.taskSeconds {
			totals[k] = v
		}
		totals[f.task] += f.lastResult.Sub(f.taskStart).Seconds()
	}
	if len(totals) == 0 {
		return nil
	}
	out := make([]TaskSummary, 0, len(totals))
	for name, seconds := range totals {
		out = append(out, TaskSummary{Task: name, Seconds: seconds, RanAt: f.ranAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Task < out[j].Task })
	return out
}

// Outputs returns what the run published through its final stats event.
func (f *SummaryFold) Outputs() map[string]any { return f.outputs }
