package run

import (
	"sort"
	"time"

	"github.com/dcadolph/yardmaster/internal/event"
)

// HostSummary is a single run's outcome for one host, derived from the run recap. It is persisted so
// cross run questions can be answered without reparsing every event.
type HostSummary struct {
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

// FailedOutcome reports whether a worst outcome counts as a failure for reliability ranking.
func FailedOutcome(worst string) bool {
	return worst == "failed" || worst == "unreachable"
}
