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

// HostSummariesFromStats builds per host summaries from the recap stats event. It returns nil when
// the run has no stats event, for example a run that never reached Ansible.
func HostSummariesFromStats(events []event.Event, ranAt time.Time) []HostSummary {
	var stats map[string]event.HostStats
	for _, e := range events {
		if e.Type == event.TypeStats && e.Stats != nil {
			stats = e.Stats
		}
	}
	if stats == nil {
		return nil
	}

	durations := hostDurations(events)
	out := make([]HostSummary, 0, len(stats))
	for host, s := range stats {
		out = append(out, HostSummary{
			Host: host, OK: s.OK, Changed: s.Changed, Failures: s.Failures,
			Unreachable: s.Unreachable, Skipped: s.Skipped, Worst: worstFromStats(s),
			DurationSeconds: durations[host], RanAt: ranAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// hostDurations estimates each host's busy seconds by attributing the gap between a task start and
// the host's result for that task to the host. Parallel strategies overlap hosts, so the estimate
// is not wall clock, but it preserves the relative weight between hosts that split balancing needs.
func hostDurations(events []event.Event) map[string]float64 {
	out := make(map[string]float64)
	var taskStart time.Time
	for _, e := range events {
		switch e.Type {
		case event.TypeTaskStart:
			taskStart = e.Time
		case event.TypeRunnerOK, event.TypeRunnerFailed, event.TypeRunnerSkipped,
			event.TypeRunnerUnreachable:
			if e.Host == "" || taskStart.IsZero() {
				continue
			}
			if gap := e.Time.Sub(taskStart).Seconds(); gap > 0 {
				out[e.Host] += gap
			}
		}
	}
	return out
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

// OutputsFromEvents returns the values the playbook published with set_stats, taken from the
// last stats event, or nil when the run published nothing.
func OutputsFromEvents(events []event.Event) map[string]any {
	var out map[string]any
	for _, e := range events {
		if e.Type == event.TypeStats && len(e.Outputs) > 0 {
			out = e.Outputs
		}
	}
	return out
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

// TaskSummariesFromEvents builds per task wall clock costs from the event stream. Each task start
// opens a block that closes at its last host result, and repeated task names accumulate. It
// returns nil when the run produced no timed task results.
func TaskSummariesFromEvents(events []event.Event, ranAt time.Time) []TaskSummary {
	totals := make(map[string]float64)
	var task string
	var taskStart, lastResult time.Time

	flush := func() {
		if task != "" && lastResult.After(taskStart) {
			totals[task] += lastResult.Sub(taskStart).Seconds()
		}
	}
	for _, e := range events {
		switch e.Type {
		case event.TypeTaskStart:
			flush()
			task, taskStart, lastResult = e.Task, e.Time, time.Time{}
		case event.TypeRunnerOK, event.TypeRunnerFailed, event.TypeRunnerSkipped,
			event.TypeRunnerUnreachable:
			if e.Time.After(lastResult) {
				lastResult = e.Time
			}
		}
	}
	flush()

	if len(totals) == 0 {
		return nil
	}
	out := make([]TaskSummary, 0, len(totals))
	for name, seconds := range totals {
		out = append(out, TaskSummary{Task: name, Seconds: seconds, RanAt: ranAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Task < out[j].Task })
	return out
}
