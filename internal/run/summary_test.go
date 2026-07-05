package run

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/dcadolph/yardmaster/internal/event"
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
			got := hostDurations(test.In)
			if diff := cmp.Diff(test.Want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("hostDurations() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
