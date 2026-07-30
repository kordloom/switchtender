package run

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/event"
)

func TestHostSummariesFromStats(t *testing.T) {
	t.Parallel()
	ranAt := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	start := ranAt.Add(time.Second)
	tests := []struct {
		In   []event.Event
		Want []HostSummary
	}{
		{ // Test 0: No stats event yields nil.
			In:   []event.Event{{Type: event.TypePlayStart, Time: start}},
			Want: nil,
		},
		{ // Test 1: Stats map to sorted summaries with durations from runner events.
			In: []event.Event{
				{Type: event.TypeTaskStart, Time: start, Task: "t1"},
				{Type: event.TypeRunnerOK, Time: start.Add(2 * time.Second), Host: "web01"},
				{Type: event.TypeRunnerFailed, Time: start.Add(3 * time.Second), Host: "db01"},
				{Type: event.TypeTaskStart, Time: start.Add(4 * time.Second), Task: "t2"},
				{Type: event.TypeRunnerOK, Time: start.Add(5 * time.Second), Host: "web01"},
				{Type: event.TypeStats, Time: start.Add(6 * time.Second), Stats: map[string]event.HostStats{
					"web01": {OK: 2},
					"db01":  {Failures: 1},
				}},
			},
			Want: []HostSummary{
				{Host: "db01", Failures: 1, Worst: "failed", DurationSeconds: 3, RanAt: ranAt},
				{Host: "web01", OK: 2, Worst: "ok", DurationSeconds: 3, RanAt: ranAt},
			},
		},
		{ // Test 2: A host in stats but without runner events has zero duration.
			In: []event.Event{
				{Type: event.TypeStats, Time: start, Stats: map[string]event.HostStats{
					"web01": {Skipped: 1},
				}},
			},
			Want: []HostSummary{
				{Host: "web01", Skipped: 1, Worst: "skipped", RanAt: ranAt},
			},
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := HostSummariesFromStats(test.In, ranAt)
			if diff := cmp.Diff(test.Want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("HostSummariesFromStats() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFlipCount(t *testing.T) {
	t.Parallel()
	sum := func(worsts ...string) []HostSummary {
		out := make([]HostSummary, len(worsts))
		for i, w := range worsts {
			out[i].Worst = w
		}
		return out
	}
	tests := []struct {
		In   []HostSummary
		Want int
	}{
		{In: nil, Want: 0},                                      // Test 0: Empty history.
		{In: sum("ok", "ok", "changed"), Want: 0},               // Test 1: Steady passing.
		{In: sum("failed", "failed"), Want: 0},                  // Test 2: Steady failing.
		{In: sum("ok", "failed", "failed"), Want: 1},            // Test 3: Fixed once.
		{In: sum("failed", "ok", "failed"), Want: 2},            // Test 4: Intermittent.
		{In: sum("ok", "unreachable", "ok", "failed"), Want: 3}, // Test 5: Unreachable counts.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := FlipCount(test.In); got != test.Want {
				t.Errorf("FlipCount() = %d, want %d", got, test.Want)
			}
		})
	}
}

func TestTaskSummariesFromEvents(t *testing.T) {
	t.Parallel()
	ranAt := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	base := ranAt.Add(time.Second)
	tests := []struct {
		In   []event.Event
		Want []TaskSummary
	}{
		{ // Test 0: No task results yields nil.
			In:   []event.Event{{Type: event.TypePlayStart, Time: base}},
			Want: nil,
		},
		{ // Test 1: Each task spans start to its last result.
			In: []event.Event{
				{Type: event.TypeTaskStart, Time: base, Task: "install"},
				{Type: event.TypeRunnerOK, Time: base.Add(time.Second), Host: "a"},
				{Type: event.TypeRunnerOK, Time: base.Add(3 * time.Second), Host: "b"},
				{Type: event.TypeTaskStart, Time: base.Add(3 * time.Second), Task: "restart"},
				{Type: event.TypeRunnerFailed, Time: base.Add(4 * time.Second), Host: "a"},
			},
			Want: []TaskSummary{
				{Task: "install", Seconds: 3, RanAt: ranAt},
				{Task: "restart", Seconds: 1, RanAt: ranAt},
			},
		},
		{ // Test 2: A repeated task name accumulates across plays.
			In: []event.Event{
				{Type: event.TypeTaskStart, Time: base, Task: "sync"},
				{Type: event.TypeRunnerOK, Time: base.Add(time.Second), Host: "a"},
				{Type: event.TypeTaskStart, Time: base.Add(10 * time.Second), Task: "sync"},
				{Type: event.TypeRunnerOK, Time: base.Add(12 * time.Second), Host: "a"},
			},
			Want: []TaskSummary{
				{Task: "sync", Seconds: 3, RanAt: ranAt},
			},
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := TaskSummariesFromEvents(test.In, ranAt)
			if diff := cmp.Diff(test.Want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("TaskSummariesFromEvents() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestHostDurations covers the per-host timing the summary fold accumulates: the gap between a task
// starting and each host reporting its result, summed across tasks.
func TestHostDurations(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		In   []event.Event
		Want map[string]float64
	}{
		{ // Test 0: No events yields no durations.
			In:   nil,
			Want: map[string]float64{},
		},
		{ // Test 1: A runner event before any task start is ignored.
			In: []event.Event{
				{Type: event.TypeRunnerOK, Time: base, Host: "a"},
			},
			Want: map[string]float64{},
		},
		{ // Test 2: Gaps accumulate per host across tasks.
			In: []event.Event{
				{Type: event.TypeTaskStart, Time: base},
				{Type: event.TypeRunnerOK, Time: base.Add(time.Second), Host: "a"},
				{Type: event.TypeRunnerOK, Time: base.Add(4 * time.Second), Host: "b"},
				{Type: event.TypeTaskStart, Time: base.Add(4 * time.Second)},
				{Type: event.TypeRunnerSkipped, Time: base.Add(4500 * time.Millisecond), Host: "a"},
				{Type: event.TypeRunnerUnreachable, Time: base.Add(6 * time.Second), Host: "b"},
			},
			Want: map[string]float64{"a": 1.5, "b": 6},
		},
		{ // Test 3: A result stamped before its task start adds nothing.
			In: []event.Event{
				{Type: event.TypeTaskStart, Time: base.Add(time.Second)},
				{Type: event.TypeRunnerOK, Time: base, Host: "a"},
			},
			Want: map[string]float64{},
		},
		{ // Test 4: Runner events without a host are ignored.
			In: []event.Event{
				{Type: event.TypeTaskStart, Time: base},
				{Type: event.TypeRunnerFailed, Time: base.Add(time.Second)},
			},
			Want: map[string]float64{},
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f := NewSummaryFold(time.Time{})
			f.Add(test.In)
			if diff := cmp.Diff(test.Want, f.hostSeconds, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("host durations mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSummaryFoldMatchesBatch checks that folding a run's events in pages gives the same answer as
// folding them all at once, at every page size. Paging is how a long run is summarized without
// holding its whole event list in memory, so the two paths have to agree, including when a page
// boundary falls between a task starting and its results arriving.
func TestSummaryFoldMatchesBatch(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	events := []event.Event{
		{Type: event.TypeTaskStart, Time: base, Task: "install"},
		{Type: event.TypeRunnerOK, Time: base.Add(time.Second), Host: "a", Task: "install"},
		{Type: event.TypeFacts, Time: base.Add(time.Second), Host: "a",
			Facts: map[string]string{"os": "debian"}},
		{Type: event.TypeRunnerOK, Time: base.Add(2 * time.Second), Host: "b", Task: "install"},
		{Type: event.TypeTaskStart, Time: base.Add(3 * time.Second), Task: "configure"},
		{Type: event.TypeRunnerFailed, Time: base.Add(5 * time.Second), Host: "a", Task: "configure"},
		{Type: event.TypeRunnerSkipped, Time: base.Add(6 * time.Second), Host: "b", Task: "configure"},
		{Type: event.TypeFacts, Time: base.Add(6 * time.Second), Host: "b",
			Facts: map[string]string{"os": "ubuntu"}},
		{Type: event.TypeStats, Time: base.Add(7 * time.Second),
			Stats:   map[string]event.HostStats{"a": {OK: 1, Failures: 1}, "b": {OK: 1, Skipped: 1}},
			Outputs: map[string]any{"version": "1.2.3"}},
	}

	whole := NewSummaryFold(base)
	whole.Add(events)

	for size := 1; size <= len(events)+1; size++ {
		t.Run(fmt.Sprintf("page size %d", size), func(t *testing.T) {
			t.Parallel()
			paged := NewSummaryFold(base)
			for i := 0; i < len(events); i += size {
				paged.Add(events[i:min(i+size, len(events))])
			}
			if diff := cmp.Diff(whole.HostSummaries(), paged.HostSummaries()); diff != "" {
				t.Errorf("host summaries mismatch (-whole +paged):\n%s", diff)
			}
			if diff := cmp.Diff(whole.TaskSummaries(), paged.TaskSummaries()); diff != "" {
				t.Errorf("task summaries mismatch (-whole +paged):\n%s", diff)
			}
			if diff := cmp.Diff(whole.HostFacts(), paged.HostFacts()); diff != "" {
				t.Errorf("host facts mismatch (-whole +paged):\n%s", diff)
			}
			if diff := cmp.Diff(whole.Outputs(), paged.Outputs()); diff != "" {
				t.Errorf("outputs mismatch (-whole +paged):\n%s", diff)
			}
		})
	}
}
