package run

import (
	"math"
	"sort"
	"time"
)

// RunBrief is the slice of a run a comparison shows: enough to name it and time it.
type RunBrief struct {
	// ID is the run identifier.
	ID string `json:"id"`
	// Status is the run's lifecycle state.
	Status Status `json:"status"`
	// Playbook is what ran.
	Playbook string `json:"playbook"`
	// Source and SourceID say what fired it.
	Source   string `json:"source,omitempty"`
	SourceID string `json:"source_id,omitempty"`
	// CreatedAt orders the pair.
	CreatedAt time.Time `json:"created_at"`
	// DurationSeconds is wall clock from start to end, absent while either is missing.
	DurationSeconds *float64 `json:"duration_seconds,omitempty"`
}

// HostComparison is one host's outcome across the two runs. A or B is nil when the run did not
// touch the host.
type HostComparison struct {
	// Host is the target host.
	Host string `json:"host"`
	// A is the host's outcome in the compared run, nil when absent.
	A *HostSummary `json:"a,omitempty"`
	// B is the host's outcome in the baseline run, nil when absent.
	B *HostSummary `json:"b,omitempty"`
	// Verdict says what happened to the host between B and A: ok, broke, recovered,
	// still_failing, added, or removed.
	Verdict string `json:"verdict"`
}

// TaskComparison is one task's timing across the two runs.
type TaskComparison struct {
	// Task is the task name.
	Task string `json:"task"`
	// ASeconds and BSeconds are the task's time in each run; negative means the run lacked it.
	ASeconds float64 `json:"a_seconds"`
	BSeconds float64 `json:"b_seconds"`
	// DeltaSeconds is A minus B for tasks both runs carry, zero otherwise.
	DeltaSeconds float64 `json:"delta_seconds"`
}

// ComparisonTotals count the host verdicts.
type ComparisonTotals struct {
	// OK, Broke, Recovered, StillFailing, Added, and Removed count hosts by verdict.
	OK           int `json:"ok"`
	Broke        int `json:"broke"`
	Recovered    int `json:"recovered"`
	StillFailing int `json:"still_failing"`
	Added        int `json:"added"`
	Removed      int `json:"removed"`
}

// Comparison is what changed between two runs: A is the run being examined and B is the baseline
// it is held against, usually the previous run of the same template.
type Comparison struct {
	// A is the run being examined and B the baseline.
	A RunBrief `json:"a"`
	B RunBrief `json:"b"`
	// SameSource reports whether both runs came from the same template, schedule, or origin, the
	// only case where a host-by-host reading is apples to apples.
	SameSource bool `json:"same_source"`
	// DurationDeltaSeconds is A's duration minus B's, absent when either is unfinished.
	DurationDeltaSeconds *float64 `json:"duration_delta_seconds,omitempty"`
	// Hosts holds every host either run touched, worst news first.
	Hosts []HostComparison `json:"hosts"`
	// Totals count the host verdicts.
	Totals ComparisonTotals `json:"totals"`
	// Tasks holds task timing changes, largest swing first, only for tasks in both runs plus
	// tasks one run gained or lost.
	Tasks []TaskComparison `json:"tasks"`
}

// verdictRank orders host verdicts worst first for display.
var verdictRank = map[string]int{
	"broke": 0, "still_failing": 1, "removed": 2, "added": 3, "recovered": 4, "ok": 5,
}

// Compare holds run a against baseline b. It is pure, so what the page and the export say is
// testable without a server.
func Compare(a, b *Run, hostsA, hostsB []HostSummary, tasksA, tasksB []TaskSummary) *Comparison {
	c := &Comparison{A: brief(a), B: brief(b)}
	c.SameSource = a.Source == b.Source && a.SourceID == b.SourceID && a.SourceID != ""
	if c.A.DurationSeconds != nil && c.B.DurationSeconds != nil {
		d := *c.A.DurationSeconds - *c.B.DurationSeconds
		c.DurationDeltaSeconds = &d
	}

	byHostA, byHostB := indexHosts(hostsA), indexHosts(hostsB)
	names := map[string]struct{}{}
	for h := range byHostA {
		names[h] = struct{}{}
	}
	for h := range byHostB {
		names[h] = struct{}{}
	}
	for name := range names {
		ha, hb := byHostA[name], byHostB[name]
		hc := HostComparison{Host: name, A: ha, B: hb}
		switch {
		case ha == nil:
			hc.Verdict = "removed"
			c.Totals.Removed++
		case hb == nil:
			hc.Verdict = "added"
			c.Totals.Added++
		case FailedOutcome(ha.Worst) && FailedOutcome(hb.Worst):
			hc.Verdict = "still_failing"
			c.Totals.StillFailing++
		case FailedOutcome(ha.Worst):
			hc.Verdict = "broke"
			c.Totals.Broke++
		case FailedOutcome(hb.Worst):
			hc.Verdict = "recovered"
			c.Totals.Recovered++
		default:
			hc.Verdict = "ok"
			c.Totals.OK++
		}
		c.Hosts = append(c.Hosts, hc)
	}
	sort.Slice(c.Hosts, func(i, j int) bool {
		ri, rj := verdictRank[c.Hosts[i].Verdict], verdictRank[c.Hosts[j].Verdict]
		if ri != rj {
			return ri < rj
		}
		return c.Hosts[i].Host < c.Hosts[j].Host
	})

	c.Tasks = compareTasks(tasksA, tasksB)
	return c
}

// brief reduces a run to what the comparison shows.
func brief(r *Run) RunBrief {
	b := RunBrief{ID: r.ID, Status: r.Status, Playbook: r.Playbook,
		Source: r.Source, SourceID: r.SourceID, CreatedAt: r.CreatedAt}
	if r.StartedAt != nil && r.EndedAt != nil {
		d := r.EndedAt.Sub(*r.StartedAt).Seconds()
		b.DurationSeconds = &d
	}
	return b
}

// indexHosts keys summaries by host, copying so the comparison owns what it points at.
func indexHosts(sums []HostSummary) map[string]*HostSummary {
	out := make(map[string]*HostSummary, len(sums))
	for i := range sums {
		cp := sums[i]
		out[cp.Host] = &cp
	}
	return out
}

// compareTasks pairs task timings, largest swing first, with gained and lost tasks included.
func compareTasks(tasksA, tasksB []TaskSummary) []TaskComparison {
	byA := make(map[string]float64, len(tasksA))
	for _, t := range tasksA {
		byA[t.Task] = t.Seconds
	}
	byB := make(map[string]float64, len(tasksB))
	for _, t := range tasksB {
		byB[t.Task] = t.Seconds
	}
	names := map[string]struct{}{}
	for n := range byA {
		names[n] = struct{}{}
	}
	for n := range byB {
		names[n] = struct{}{}
	}
	var out []TaskComparison
	for name := range names {
		sa, inA := byA[name]
		sb, inB := byB[name]
		tc := TaskComparison{Task: name, ASeconds: -1, BSeconds: -1}
		if inA {
			tc.ASeconds = sa
		}
		if inB {
			tc.BSeconds = sb
		}
		if inA && inB {
			tc.DeltaSeconds = sa - sb
		}
		out = append(out, tc)
	}
	sort.Slice(out, func(i, j int) bool {
		di, dj := math.Abs(out[i].DeltaSeconds), math.Abs(out[j].DeltaSeconds)
		if di != dj {
			return di > dj
		}
		return out[i].Task < out[j].Task
	})
	return out
}
