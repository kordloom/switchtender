package run

import (
	"testing"
	"time"
)

// timeAt returns a fixed time offset by minutes, for readable test wiring.
func timeAt(min int) *time.Time {
	t := time.Date(2026, 8, 4, 12, min, 0, 0, time.UTC)
	return &t
}

func TestCompareNamesEveryHostVerdict(t *testing.T) {
	t.Parallel()
	a := &Run{ID: "run_a", Status: StatusFailed, Playbook: "site.yml", Source: "template",
		SourceID: "tpl_1", CreatedAt: *timeAt(10), StartedAt: timeAt(10), EndedAt: timeAt(16)}
	b := &Run{ID: "run_b", Status: StatusSucceeded, Playbook: "site.yml", Source: "template",
		SourceID: "tpl_1", CreatedAt: *timeAt(0), StartedAt: timeAt(0), EndedAt: timeAt(4)}
	hostsA := []HostSummary{
		{Host: "web01", Worst: "failed", Failures: 2},
		{Host: "web02", Worst: "ok", OK: 5},
		{Host: "db01", Worst: "unreachable", Unreachable: 1},
		{Host: "new01", Worst: "ok", OK: 1},
	}
	hostsB := []HostSummary{
		{Host: "web01", Worst: "ok", OK: 5},
		{Host: "web02", Worst: "changed", Changed: 2},
		{Host: "db01", Worst: "failed", Failures: 1},
		{Host: "old01", Worst: "ok", OK: 1},
	}
	tasksA := []TaskSummary{{Task: "deploy", Seconds: 30}, {Task: "migrate", Seconds: 4}}
	tasksB := []TaskSummary{{Task: "deploy", Seconds: 10}, {Task: "cleanup", Seconds: 2}}

	c := Compare(a, b, hostsA, hostsB, tasksA, tasksB)
	if !c.SameSource {
		t.Error("SameSource = false for two runs of one template")
	}
	if c.DurationDeltaSeconds == nil || *c.DurationDeltaSeconds != 120 {
		t.Errorf("duration delta = %v, want the two extra minutes", c.DurationDeltaSeconds)
	}
	verdicts := map[string]string{}
	for _, h := range c.Hosts {
		verdicts[h.Host] = h.Verdict
	}
	want := map[string]string{
		"web01": "broke", "web02": "ok", "db01": "still_failing",
		"new01": "added", "old01": "removed",
	}
	for host, w := range want {
		if verdicts[host] != w {
			t.Errorf("%s verdict = %q, want %q", host, verdicts[host], w)
		}
	}
	if c.Totals != (ComparisonTotals{OK: 1, Broke: 1, StillFailing: 1, Added: 1, Removed: 1}) {
		t.Errorf("totals = %+v, want each verdict counted once", c.Totals)
	}
	// The worst news leads.
	if c.Hosts[0].Verdict != "broke" {
		t.Errorf("first row verdict = %q, want the broken host first", c.Hosts[0].Verdict)
	}
	// The biggest task swing leads, and one-sided tasks are carried with a -1 marker.
	if c.Tasks[0].Task != "deploy" || c.Tasks[0].DeltaSeconds != 20 {
		t.Errorf("tasks[0] = %+v, want deploy's 20s swing first", c.Tasks[0])
	}
	oneSided := map[string]TaskComparison{}
	for _, tc := range c.Tasks {
		oneSided[tc.Task] = tc
	}
	if oneSided["migrate"].BSeconds != -1 || oneSided["cleanup"].ASeconds != -1 {
		t.Errorf("one-sided tasks = %+v / %+v, want the missing side marked",
			oneSided["migrate"], oneSided["cleanup"])
	}
}

func TestCompareRecoveredAndUnfinished(t *testing.T) {
	t.Parallel()
	a := &Run{ID: "run_a", Status: StatusSucceeded, CreatedAt: *timeAt(10)}
	b := &Run{ID: "run_b", Status: StatusFailed, CreatedAt: *timeAt(0), StartedAt: timeAt(0)}
	c := Compare(a, b,
		[]HostSummary{{Host: "web01", Worst: "ok"}},
		[]HostSummary{{Host: "web01", Worst: "failed"}}, nil, nil)
	if c.Hosts[0].Verdict != "recovered" || c.Totals.Recovered != 1 {
		t.Errorf("verdict = %+v, want the recovery named", c.Hosts[0])
	}
	// B never ended: no duration delta is invented.
	if c.DurationDeltaSeconds != nil {
		t.Errorf("duration delta = %v, want absent for an unfinished baseline", *c.DurationDeltaSeconds)
	}
	if c.SameSource {
		t.Error("SameSource = true for two runs with no source at all")
	}
}
