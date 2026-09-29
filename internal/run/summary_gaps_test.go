package run

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/event"
)

// TestWorstOutcomeRanksBySeverity pins which of a host's counters decides the one word its run is
// remembered by.
//
// The word is what fleet health ranks on, what a comparison reads to call a host broken or
// recovered, and what a sparkline shows. A host that both failed a task and changed another must
// read as failed, not as changed, or a broken host disappears from the ranking that exists to find
// it.
func TestWorstOutcomeRanksBySeverity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		Stats     event.HostStats
		WantWorst string
	}{
		{Name: "nothing at all", Stats: event.HostStats{}, WantWorst: "skipped"}, // Test 0: Empty.
		{Name: "only skipped", Stats: event.HostStats{Skipped: 3},
			WantWorst: "skipped"}, // Test 1: Nothing happened.
		{Name: "only ok", Stats: event.HostStats{OK: 3}, WantWorst: "ok"}, // Test 2: Clean.
		{Name: "changed beats ok", Stats: event.HostStats{OK: 3, Changed: 1},
			WantWorst: "changed"}, // Test 3: A change is the news.
		{Name: "unreachable beats changed", Stats: event.HostStats{Changed: 1, Unreachable: 1},
			WantWorst: "unreachable"}, // Test 4: The host was not there.
		{Name: "failed beats unreachable", Stats: event.HostStats{Unreachable: 1, Failures: 1},
			WantWorst: "failed"}, // Test 5: A real failure leads.
		{Name: "failed beats everything",
			Stats:     event.HostStats{OK: 9, Changed: 9, Skipped: 9, Unreachable: 9, Failures: 1},
			WantWorst: "failed"}, // Test 6: One failure among many successes still reads failed.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := HostSummariesFromStats([]event.Event{{
				Type: event.TypeStats, Stats: map[string]event.HostStats{"web01": test.Stats},
			}}, time.Time{})
			if len(got) != 1 {
				t.Fatalf("HostSummariesFromStats() returned %d summaries, want one", len(got))
			}
			if got[0].Worst != test.WantWorst {
				t.Errorf("worst = %q, want %q for %+v", got[0].Worst, test.WantWorst, test.Stats)
			}
			// The reliability ranking reads the same word, so the two must agree.
			wantFailed := test.WantWorst == "failed" || test.WantWorst == "unreachable"
			if FailedOutcome(got[0].Worst) != wantFailed {
				t.Errorf("FailedOutcome(%q) = %v, want %v", got[0].Worst,
					FailedOutcome(got[0].Worst), wantFailed)
			}
		})
	}
}

// TestFailedOutcomeNamesOnlyTheTwoFailures pins exactly which words count as a failure, since the
// ranking, the flip count, and the comparison verdicts all key on it.
func TestFailedOutcomeNamesOnlyTheTwoFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Worst      string
		WantFailed bool
	}{
		{Worst: "failed", WantFailed: true},      // Test 0: The tool returned an error.
		{Worst: "unreachable", WantFailed: true}, // Test 1: The host was not there.
		{Worst: "ok", WantFailed: false},         // Test 2: Clean.
		{Worst: "changed", WantFailed: false},    // Test 3: A change is not a failure.
		{Worst: "skipped", WantFailed: false},    // Test 4: Nothing ran.
		{Worst: "", WantFailed: false},           // Test 5: No outcome is not a failure.
		{Worst: "FAILED", WantFailed: false},     // Test 6: The match is exact, not folded.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %q", testNum, test.Worst), func(t *testing.T) {
			t.Parallel()
			if got := FailedOutcome(test.Worst); got != test.WantFailed {
				t.Errorf("FailedOutcome(%q) = %v, want %v", test.Worst, got, test.WantFailed)
			}
		})
	}
}

// TestFlipOutcomesAgreesWithFlipCount pins that the two ways of counting flakiness reach the same
// answer.
//
// A view that recomputes a host's flakiness over a filtered window holds outcome strings, while the
// store holds whole summaries. If the two counted differently the same host would read flaky on one
// page and steady on another, which is exactly the kind of disagreement that makes an operator stop
// trusting the number.
func TestFlipOutcomesAgreesWithFlipCount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Outcomes  []string
		WantFlips int
	}{
		{Outcomes: nil, WantFlips: 0},                                  // Test 0: No history.
		{Outcomes: []string{"ok"}, WantFlips: 0},                       // Test 1: One run cannot flip.
		{Outcomes: []string{"failed"}, WantFlips: 0},                   // Test 2: Same.
		{Outcomes: []string{"ok", "changed", "skipped"}, WantFlips: 0}, // Test 3: All passing.
		{Outcomes: []string{"failed", "unreachable"}, WantFlips: 0},    // Test 4: Both are failures,
		// so switching between them is not a flip.
		{Outcomes: []string{"ok", "failed"}, WantFlips: 1},                 // Test 5: One break.
		{Outcomes: []string{"failed", "ok", "failed", "ok"}, WantFlips: 3}, // Test 6: Flapping.
		{Outcomes: []string{"", "failed"}, WantFlips: 1},                   // Test 7: An empty
		// outcome counts as passing, so the break is still seen.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := FlipOutcomes(test.Outcomes); got != test.WantFlips {
				t.Errorf("FlipOutcomes(%v) = %d, want %d", test.Outcomes, got, test.WantFlips)
			}
			summaries := make([]HostSummary, len(test.Outcomes))
			for i, o := range test.Outcomes {
				summaries[i].Worst = o
			}
			if got := FlipCount(summaries); got != test.WantFlips {
				t.Errorf("FlipCount() = %d, want %d, disagreeing with FlipOutcomes", got,
					test.WantFlips)
			}
		})
	}
}

// TestBatchFoldsMatchTheStreamingFold pins that the three batch helpers give the same answers the
// fold does, since they are the paths a caller uses when it already holds the whole event list.
//
// They exist as thin wrappers precisely so the two cannot drift, and a wrapper that reduced over
// the events itself is what drifting looks like.
func TestBatchFoldsMatchTheStreamingFold(t *testing.T) {
	t.Parallel()
	ranAt := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	events := []event.Event{
		{Type: event.TypeTaskStart, Time: ranAt, Task: "install"},
		{Type: event.TypeRunnerOK, Time: ranAt.Add(time.Second), Host: "web01"},
		{Type: event.TypeFacts, Time: ranAt.Add(time.Second), Host: "web01",
			Facts: map[string]string{"distribution": "Debian"}},
		{Type: event.TypeStats, Time: ranAt.Add(2 * time.Second),
			Stats:   map[string]event.HostStats{"web01": {OK: 1}},
			Outputs: map[string]any{"version": "1.2.3"}},
	}
	fold := NewSummaryFold(ranAt)
	fold.Add(events)

	if diff := cmp.Diff(fold.HostFacts(), HostFactsFromEvents(events, ranAt),
		cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("HostFactsFromEvents disagrees with the fold (-fold +batch):\n%s", diff)
	}
	if diff := cmp.Diff(fold.HostSummaries(), HostSummariesFromStats(events, ranAt),
		cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("HostSummariesFromStats disagrees with the fold (-fold +batch):\n%s", diff)
	}
	if diff := cmp.Diff(fold.TaskSummaries(), TaskSummariesFromEvents(events, ranAt),
		cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("TaskSummariesFromEvents disagrees with the fold (-fold +batch):\n%s", diff)
	}
	// Outputs are read without a ranAt, since they carry no timestamp of their own.
	bare := NewSummaryFold(time.Time{})
	bare.Add(events)
	if diff := cmp.Diff(bare.Outputs(), OutputsFromEvents(events), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("OutputsFromEvents disagrees with the fold (-fold +batch):\n%s", diff)
	}
}

// TestAFoldWithNothingInItAnswersNothing pins that a run that reported no stats, gathered no facts,
// and finished no task produces empty summaries rather than zero-valued rows.
//
// A summary row invented for a run that produced none would enter fleet health, drift, and task
// trends as a real observation, which is worse than the run simply not appearing.
func TestAFoldWithNothingInItAnswersNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		In   []event.Event
	}{
		{Name: "no events at all", In: nil}, // Test 0: Nothing arrived.
		{Name: "only a play start",
			In: []event.Event{{Type: event.TypePlayStart, Time: time.Now()}}, // Test 1: Started
			// and produced nothing.
		},
		{Name: "a stats event carrying nothing",
			In: []event.Event{{Type: event.TypeStats, Time: time.Now()}}, // Test 2: The event
			// arrived with a nil map, which must not be read as a run with no hosts.
		},
		{Name: "facts with no host",
			In: []event.Event{{Type: event.TypeFacts, Time: time.Now(),
				Facts: map[string]string{"os": "debian"}}}, // Test 3: Nothing to key on.
		},
		{Name: "facts with no facts",
			In: []event.Event{{Type: event.TypeFacts, Time: time.Now(), Host: "web01"}}, // Test 4:
			// A gather that returned nothing must not blank a host's stored facts.
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			f := NewSummaryFold(time.Now())
			f.Add(test.In)
			if got := f.HostSummaries(); got != nil {
				t.Errorf("HostSummaries() = %+v, want nil", got)
			}
			if got := f.HostFacts(); got != nil {
				t.Errorf("HostFacts() = %+v, want nil", got)
			}
			if got := f.TaskSummaries(); got != nil {
				t.Errorf("TaskSummaries() = %+v, want nil", got)
			}
			if got := f.Outputs(); got != nil {
				t.Errorf("Outputs() = %+v, want nil", got)
			}
		})
	}
}

// TestTaskSummariesDoesNotMutateTheFold pins that asking for the task durations mid-stream banks
// the open task into the answer without banking it into the fold, so folding may continue.
//
// A caller reporting progress asks for the summaries and then keeps feeding events. If the read
// banked the open task, the same seconds would be counted again when the task actually closed, and
// a long task read twice would report double its real duration in the trend view.
func TestTaskSummariesDoesNotMutateTheFold(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	f := NewSummaryFold(base)
	f.Add([]event.Event{
		{Type: event.TypeTaskStart, Time: base, Task: "install"},
		{Type: event.TypeRunnerOK, Time: base.Add(2 * time.Second), Host: "web01"},
	})

	first := f.TaskSummaries()
	second := f.TaskSummaries()
	if diff := cmp.Diff(first, second, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("reading twice gave different answers (-first +second):\n%s", diff)
	}
	if len(first) != 1 || first[0].Seconds != 2 {
		t.Fatalf("TaskSummaries() = %+v, want the open task banked at two seconds", first)
	}

	// Folding continues, and the task's final duration is not the sum of the reads.
	f.Add([]event.Event{
		{Type: event.TypeRunnerOK, Time: base.Add(5 * time.Second), Host: "web02"},
		{Type: event.TypeTaskStart, Time: base.Add(5 * time.Second), Task: "configure"},
		{Type: event.TypeRunnerOK, Time: base.Add(6 * time.Second), Host: "web01"},
	})
	want := []TaskSummary{
		{Task: "configure", Seconds: 1, RanAt: base},
		{Task: "install", Seconds: 5, RanAt: base},
	}
	if diff := cmp.Diff(want, f.TaskSummaries(), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("task durations after continuing (-want +got):\n%s", diff)
	}
}

// TestTheLastStatsEventWins pins that a run reporting stats more than once is summarized by its
// final report, which is the whole-run recap rather than an intermediate one.
func TestTheLastStatsEventWins(t *testing.T) {
	t.Parallel()
	ranAt := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	got := HostSummariesFromStats([]event.Event{
		{Type: event.TypeStats, Time: ranAt,
			Stats: map[string]event.HostStats{"web01": {OK: 1}}},
		{Type: event.TypeStats, Time: ranAt.Add(time.Second),
			Stats: map[string]event.HostStats{"web01": {OK: 5, Failures: 1}}},
	}, ranAt)
	want := []HostSummary{{Host: "web01", OK: 5, Failures: 1, Worst: "failed", RanAt: ranAt}}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("host summaries (-want +got):\n%s", diff)
	}

	// A later stats event carrying no outputs leaves the outputs the earlier one published, so a
	// pipeline step's downstream inputs are not lost by a second recap.
	outputs := OutputsFromEvents([]event.Event{
		{Type: event.TypeStats, Time: ranAt, Outputs: map[string]any{"version": "1.2.3"}},
		{Type: event.TypeStats, Time: ranAt.Add(time.Second),
			Stats: map[string]event.HostStats{"web01": {OK: 1}}},
	})
	if diff := cmp.Diff(map[string]any{"version": "1.2.3"}, outputs,
		cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("published outputs (-want +got):\n%s", diff)
	}
}

// TestSummaryFoldDropsATaskThatNeverReported pins that a task which starts and produces no result
// banks no time, rather than banking the distance from its start back to the zero time.
//
// A task can end without a result: it is skipped by a conditional on every host, or the run is
// interrupted between the start and the first result. The fold resets its last-result marker at
// every task start, so without the guard the next task start closes the silent one with a
// subtraction against the zero time. That is roughly minus fifty-eight years of seconds, and it
// lands in the same task trend an operator reads to find work that is getting slower.
func TestSummaryFoldDropsATaskThatNeverReported(t *testing.T) {
	t.Parallel()
	ranAt := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	events := []event.Event{
		// A task that starts and reports nothing at all before the next one begins.
		{Type: event.TypeTaskStart, Time: ranAt, Task: "gather facts"},
		{Type: event.TypeTaskStart, Time: ranAt.Add(2 * time.Second), Task: "install"},
		{Type: event.TypeRunnerOK, Time: ranAt.Add(5 * time.Second), Task: "install", Host: "web01"},
	}
	got := TaskSummariesFromEvents(events, ranAt)
	want := []TaskSummary{{Task: "install", Seconds: 3, RanAt: ranAt}}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("task summaries (-want +got):\n%s", diff)
	}
	for _, ts := range got {
		if ts.Seconds < 0 {
			t.Errorf("task %q banked %v seconds: a task that never reported was closed against the "+
				"zero time", ts.Task, ts.Seconds)
		}
	}
}
