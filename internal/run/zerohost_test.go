package run_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
)

// TestSummaryFoldRecapSeparatesAnEmptyRecapFromNone pins the signal that decides whether a run
// touched a host.
//
// Ansible exits zero when a host pattern matches nothing: it warns, prints a recap naming no host,
// and stops. A dispatcher reading the exit code alone recorded that as succeeded, signed it, and
// linked it into the chain, so the proof was valid and it was a proof of nothing. The fix reads the
// recap instead, and the recap only answers the question if these three states stay distinct: a
// recap naming hosts, a recap naming none, and no recap at all.
//
// The last two are the pair that matters. A recap naming no host is proof the run touched none. No
// recap at all is what a run with event capture unavailable leaves behind, and it proves nothing
// either way. Collapsing them would fail runs nobody can say anything about.
//
// The outcome this feeds is pinned in internal/dispatch/zerohost_test.go, where the dispatcher
// decides a run's terminal status. That is where the defect was, so that is where the end-to-end
// test lives.
func TestSummaryFoldRecapSeparatesAnEmptyRecapFromNone(t *testing.T) {
	t.Parallel()
	ranAt := time.Unix(1719000000, 0).UTC()
	tests := []struct {
		Name          string
		Events        []event.Event
		WantHosts     int
		WantReported  bool
		WantSummaries int
	}{{ // Test 0: The host pattern matched nothing, so the recap arrived naming no host.
		Name: "recap names no host",
		Events: []event.Event{
			{Type: event.TypePlayStart, Time: ranAt, Play: "nothing"},
			{Type: event.TypeStats, Time: ranAt, Stats: map[string]event.HostStats{}},
		},
		WantHosts: 0, WantReported: true, WantSummaries: 0,
	}, { // Test 1: A real host that skipped every task is still named in the recap.
		Name: "recap names a host that skipped every task",
		Events: []event.Event{
			{Type: event.TypeRunnerSkipped, Time: ranAt, Host: "web01", Task: "never runs"},
			{Type: event.TypeStats, Time: ranAt, Stats: map[string]event.HostStats{
				"web01": {Skipped: 1},
			}},
		},
		WantHosts: 1, WantReported: true, WantSummaries: 1,
	}, { // Test 2: An ordinary run naming two hosts.
		Name: "recap names two hosts",
		Events: []event.Event{
			{Type: event.TypeStats, Time: ranAt, Stats: map[string]event.HostStats{
				"web01": {OK: 1, Changed: 1}, "web02": {OK: 1},
			}},
		},
		WantHosts: 2, WantReported: true, WantSummaries: 2,
	}, { // Test 3: No recap at all, which is what event capture being unavailable leaves.
		Name: "no recap", Events: nil, WantHosts: 0, WantReported: false, WantSummaries: 0,
	}, { // Test 4: Events with no stats event among them report no recap either.
		Name: "events but no recap",
		Events: []event.Event{
			{Type: event.TypePlayStart, Time: ranAt, Play: "site"},
			{Type: event.TypeRunnerOK, Time: ranAt, Host: "web01", Task: "install"},
		},
		WantHosts: 0, WantReported: false, WantSummaries: 0,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			fold := run.NewSummaryFold(ranAt)
			fold.Add(test.Events)

			hosts, reported := fold.Recap()
			if diff := cmp.Diff(test.WantHosts, hosts, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("recap host count mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantReported, reported, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("recap reported mismatch (-want +got):\n%s", diff)
			}
			// The count has to agree with the summaries the same fold produces, or the outcome
			// decision and the stored evidence would be reading two different runs.
			got := fold.HostSummaries()
			if diff := cmp.Diff(test.WantSummaries, len(got), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("host summary count mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestParsedEmptyRecapIsReportedNotAbsent pins the wire behavior the whole fix rests on: the recap
// the callback plugin writes for a run that matched no host is the JSON object "stats": {}, and it
// has to survive parsing as a reported recap naming nobody rather than as no recap at all. If
// encoding/json ever handed back a nil map for it, a zero-host run would read as unproven and go
// back to being recorded as a success.
func TestParsedEmptyRecapIsReportedNotAbsent(t *testing.T) {
	t.Parallel()
	// Written by ansible core 2.21.1 with the callback plugin in internal/roundhouse/plugins, for a
	// playbook whose host pattern matched nothing.
	line := []byte(`{"type":"stats","ts":1719000001,"stats":{}}`)

	e, ok, err := event.ParseLine(line)
	if err != nil {
		t.Fatalf("ParseLine() error = %v", err)
	}
	if !ok {
		t.Fatal("ParseLine() reported no event for the recap line")
	}
	if e.Stats == nil {
		t.Fatal("an empty recap parsed to a nil stats map, so a zero-host run would read as unproven")
	}
	if len(e.Stats) != 0 {
		t.Errorf("recap named %d host(s), want none", len(e.Stats))
	}

	fold := run.NewSummaryFold(time.Unix(1719000001, 0).UTC())
	fold.Add([]event.Event{e})
	hosts, reported := fold.Recap()
	if hosts != 0 || !reported {
		t.Errorf("Recap() = (%d, %t), want (0, true)", hosts, reported)
	}
}
