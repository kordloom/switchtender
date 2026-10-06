package storetest

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// randomName returns at least n bytes of runes drawn at random from lo through hi, always valid
// UTF-8, since a surrogate drawn from the range is written as the replacement character. Random
// text is what reaches the index limit: PostgreSQL compresses a long key before indexing it, so a
// repetitive name of the same length slips under the limit and proves nothing.
func randomName(rng *rand.Rand, n int, lo, hi rune) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteRune(lo + rune(rng.IntN(int(hi-lo+1))))
	}
	return b.String()
}

// testSummaryLongNames pins that a host or task name longer than an index entry can hold is stored
// on every backend in its run.SummaryName form, that an ordinary name beside it is stored exactly
// as given, and that every lookup finds the long one again by the name it arrived with.
//
// The fleet tables index host and task names, and PostgreSQL refuses an index entry longer than
// about 2700 bytes. A name is somebody else's, from an inventory or a playbook, so a pathological
// one failed the whole summary write on PostgreSQL while SQLite kept it whole: the run finished and
// was missing from fleet health, drift, host history, and task trends on one backend only.
//
// The cases share the one store the contract hands this test, and the PostgreSQL contract truncates
// every table each time it makes a store, so they run in order rather than in parallel.
func testSummaryLongNames(t *testing.T, store run.Store) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	rng := rand.New(rand.NewPCG(2026, 10))
	tests := []struct {
		Host string
		Task string
	}{{ // Test 0: Random ASCII far past every index limit.
		Host: randomName(rng, 10000, '!', '~'), Task: randomName(rng, 10000, '!', '~'),
	}, { // Test 1: Random ASCII past the index entry limit and inside one page.
		Host: randomName(rng, 3000, '!', '~'), Task: randomName(rng, 3000, '!', '~'),
	}, { // Test 2: Random three-byte runes, which have to be cut on a rune boundary.
		Host: randomName(rng, 10000, 0x4e00, 0x9fff), Task: randomName(rng, 10000, 0x4e00, 0x9fff),
	}, { // Test 3: Random four-byte runes for the host and runes of every width for the task.
		Host: randomName(rng, 10000, 0x1f300, 0x1f5ff), Task: randomName(rng, 10000, 0x80, 0x10ffff),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			runID := fmt.Sprintf("rlong%d", testNum)
			checkLongNames(ctx, t, store, runID, base.Add(time.Duration(testNum)*time.Minute),
				test.Host, test.Task)
		})
	}

	// Two long names that share their whole start are still two hosts, kept apart by the digest
	// that ends each one.
	shared := randomName(rng, 4000, '!', '~')
	first, second := shared+"-first", shared+"-second"
	if err := store.SaveHostSummary(ctx, "rtwin1", []run.HostSummary{
		{Host: first, OK: 1, Worst: "ok", RanAt: base},
	}); err != nil {
		t.Fatalf("SaveHostSummary(first) error = %v", err)
	}
	if err := store.SaveHostSummary(ctx, "rtwin2", []run.HostSummary{
		{Host: second, OK: 1, Worst: "ok", RanAt: base},
	}); err != nil {
		t.Fatalf("SaveHostSummary(second) error = %v", err)
	}
	if run.SummaryName(first) == run.SummaryName(second) {
		t.Fatalf("two long names sharing a start were cut to the same name")
	}
	for name, want := range map[string]string{first: "rtwin1", second: "rtwin2"} {
		history, err := store.HostHistory(ctx, name, 10)
		if err != nil {
			t.Fatalf("HostHistory() error = %v", err)
		}
		got := make([]string, 0, len(history))
		for _, hs := range history {
			got = append(got, hs.RunID)
		}
		if diff := cmp.Diff([]string{want}, got, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("HostHistory() of one of two names sharing a start mismatch (-want +got):\n%s",
				diff)
		}
	}
}

// checkLongNames saves one run's summaries and facts with a long host and task name beside ordinary
// ones, then checks what every backend stores and that each lookup finds the run by the long names.
func checkLongNames(ctx context.Context, t *testing.T, store run.Store, runID string, at time.Time,
	host, task string) {
	t.Helper()
	if len(host) <= run.MaxSummaryNameBytes || len(task) <= run.MaxSummaryNameBytes {
		t.Fatalf("test names are %d and %d bytes, want both over %d", len(host), len(task),
			run.MaxSummaryNameBytes)
	}
	// Summaries attach while the run is live, since the terminal fence drops them afterward, and
	// the run has to exist for the run list's host and task filters to find it.
	live := &run.Run{ID: runID, Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: at}
	if err := store.Save(ctx, live); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.SaveHostSummary(ctx, runID, []run.HostSummary{
		{Host: host, OK: 1, Worst: "ok", RanAt: at},
		{Host: "web01", OK: 2, Worst: "ok", RanAt: at},
	}); err != nil {
		t.Fatalf("SaveHostSummary() refused a long host name: %v", err)
	}
	if err := store.SaveTaskSummary(ctx, runID, []run.TaskSummary{
		{Task: task, Seconds: 1.5, RanAt: at},
		{Task: "copy config", Seconds: 2, RanAt: at},
	}); err != nil {
		t.Fatalf("SaveTaskSummary() refused a long task name: %v", err)
	}
	if err := store.SaveHostFacts(ctx, runID, []run.HostFacts{
		{Host: host, Facts: map[string]string{"distro": "debian"}, GatheredAt: at},
	}); err != nil {
		t.Fatalf("SaveHostFacts() refused a long host name: %v", err)
	}

	storedHost, storedTask := run.SummaryName(host), run.SummaryName(task)
	for _, name := range []string{storedHost, storedTask} {
		if len(name) > run.MaxSummaryNameBytes || !utf8.ValidString(name) {
			t.Fatalf("stored name is %d bytes, valid UTF-8 %v, want at most %d and valid", len(name),
				utf8.ValidString(name), run.MaxSummaryNameBytes)
		}
	}

	hosts, err := store.RunHostSummaries(ctx, runID)
	if err != nil {
		t.Fatalf("RunHostSummaries() error = %v", err)
	}
	gotHosts := make([]string, 0, len(hosts))
	for _, hs := range hosts {
		gotHosts = append(gotHosts, hs.Host)
	}
	slices.Sort(gotHosts)
	wantHosts := []string{storedHost, "web01"}
	slices.Sort(wantHosts)
	if diff := cmp.Diff(wantHosts, gotHosts, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("stored hosts mismatch, want the long one cut and web01 exact (-want +got):\n%s",
			diff)
	}
	tasks, err := store.RunTaskSummaries(ctx, runID)
	if err != nil {
		t.Fatalf("RunTaskSummaries() error = %v", err)
	}
	gotTasks := make([]string, 0, len(tasks))
	for _, ts := range tasks {
		gotTasks = append(gotTasks, ts.Task)
	}
	slices.Sort(gotTasks)
	wantTasks := []string{storedTask, "copy config"}
	slices.Sort(wantTasks)
	if diff := cmp.Diff(wantTasks, gotTasks, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("stored tasks mismatch, want the long one cut and the short one exact "+
			"(-want +got):\n%s", diff)
	}

	// A lookup by the name the run arrived with and by the name the fleet views display both find
	// it.
	for _, name := range []string{host, storedHost} {
		history, err := store.HostHistory(ctx, name, 10)
		if err != nil {
			t.Fatalf("HostHistory() error = %v", err)
		}
		if len(history) != 1 || history[0].RunID != runID || history[0].Host != storedHost {
			t.Errorf("HostHistory() of a long host = %d rows, want this run under the cut name",
				len(history))
		}
		facts, err := store.HostFactsFor(ctx, name)
		if err != nil {
			t.Fatalf("HostFactsFor() error = %v", err)
		}
		if facts.Facts["distro"] != "debian" || facts.Host != storedHost {
			t.Errorf("HostFactsFor() of a long host = %v, want its gathered facts", facts.Facts)
		}
	}
	for _, filter := range []run.ListFilter{{Host: host}, {Host: storedHost}, {Task: task},
		{Task: storedTask}} {
		hit, err := store.ListPage(ctx, filter, 0, 0)
		if err != nil {
			t.Fatalf("ListPage() error = %v", err)
		}
		if diff := cmp.Diff([]string{runID}, runIDs(hit), cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("run list filtered by a long name mismatch (-want +got):\n%s", diff)
		}
	}
}

// LongNamesStoredWhole checks a store holding rows an earlier release wrote for runID with host and
// task stored whole, before names too long to index were cut. The rows must read back unchanged and
// every lookup by those names must still find them, since history is never rewritten and a lookup
// that matched only the cut form would hide it. The caller writes the rows with plain SQL, the way
// that release did, because no write path through the store produces them any more.
func LongNamesStoredWhole(t *testing.T, store run.Store, runID, host, task string) {
	t.Helper()
	ctx := context.Background()
	hosts, err := store.RunHostSummaries(ctx, runID)
	if err != nil {
		t.Fatalf("RunHostSummaries() error = %v", err)
	}
	if len(hosts) != 1 || hosts[0].Host != host {
		t.Errorf("RunHostSummaries() = %d rows, want the one host stored whole and unchanged",
			len(hosts))
	}
	tasks, err := store.RunTaskSummaries(ctx, runID)
	if err != nil {
		t.Fatalf("RunTaskSummaries() error = %v", err)
	}
	if len(tasks) != 1 || tasks[0].Task != task {
		t.Errorf("RunTaskSummaries() = %d rows, want the one task stored whole and unchanged",
			len(tasks))
	}
	history, err := store.HostHistory(ctx, host, 10)
	if err != nil {
		t.Fatalf("HostHistory() error = %v", err)
	}
	if len(history) != 1 || history[0].RunID != runID || history[0].Host != host {
		t.Errorf("HostHistory() of a host stored whole = %d rows, want its one run", len(history))
	}
	facts, err := store.HostFactsFor(ctx, host)
	if err != nil {
		t.Fatalf("HostFactsFor() of a host stored whole error = %v", err)
	}
	if facts.Host != host || facts.RunID != runID {
		t.Errorf("HostFactsFor() of a host stored whole = run %q, want %q", facts.RunID, runID)
	}
	for _, filter := range []run.ListFilter{{Host: host}, {Task: task}} {
		hit, err := store.ListPage(ctx, filter, 0, 0)
		if err != nil {
			t.Fatalf("ListPage() error = %v", err)
		}
		if diff := cmp.Diff([]string{runID}, runIDs(hit), cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("run list filtered by a name stored whole mismatch (-want +got):\n%s", diff)
		}
	}
}
