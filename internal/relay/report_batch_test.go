package relay_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// storeBackend names a store a relay case runs against.
type storeBackend struct {
	// Name identifies the backend in the subtest.
	Name string
	// Open returns a fresh store of that kind.
	Open func(*testing.T) run.Store
}

// storeBackends returns the stores a control node actually runs on, so a case about what the control
// node records is not decided by the in-memory store alone.
func storeBackends() []storeBackend {
	return []storeBackend{{
		Name: "memory",
		Open: func(*testing.T) run.Store { return run.NewMemStore() },
	}, {
		Name: "sqlite",
		Open: func(t *testing.T) run.Store {
			t.Helper()
			db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			return db.Runs()
		},
	}}
}

// relayOver stands up a relay server and client over the given store.
func relayOver(t *testing.T, backing run.Store) (*relay.Client, run.Store) {
	t.Helper()
	ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, nil))
	t.Cleanup(ts.Close)
	return relay.NewClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, ts.Client())), backing
}

// TestARelayReportsEveryHostAndTask covers a silent undercount in the record the product's evidence is
// built from.
//
// A worker sends its per-host outcomes and per-task timings in batches of a thousand, because the
// control node refuses one call carrying more than a few thousand items. The endpoint those batches
// arrive at replaces the run's stored summaries with the body it was given, so each batch deleted the
// rows the batch before it had written and only the last partial one survived: a playbook over 1500
// hosts stored 500 of them.
//
// It is not only a display problem. The run's committed outcome record is built from these stored
// summaries, so the receipt cryptographically commits to the undercount, and the run's own facts for
// hosts in the discarded batches were then refused with "this run has recorded no result for host X".
// Fleet health, drift, host history, and failed-host relaunch all read the same short list. Nothing
// errors anywhere, which is what makes it dangerous: the numbers are simply wrong, signed, and
// consistent with each other.
//
// The in-process path never had this, so it only appears on the deployment shape the product tells
// customers to scale into.
func TestARelayReportsEveryHostAndTask(t *testing.T) {
	t.Parallel()
	// Both stores, because the fix reads back what a run has already reported and merges the next batch
	// over it, and read-back order and replace semantics are exactly the kind of thing the in-memory
	// store and a real SQL one disagree about.
	for _, backend := range storeBackends() {
		t.Run(backend.Name, func(t *testing.T) {
			t.Parallel()
			reportsEveryHostAndTask(t, backend.Open)
		})
	}
}

// reportsEveryHostAndTask is TestARelayReportsEveryHostAndTask against one store.
func reportsEveryHostAndTask(t *testing.T, open func(*testing.T) run.Store) {
	t.Helper()
	ctx := context.Background()
	c, backing := relayOver(t, open(t))

	// A run the worker holds and is still executing, so its reports are not fenced.
	claimed := time.Now()
	r := &run.Run{
		ID: "run_wide", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: claimed,
		ClaimedBy: "worker-a", ClaimedAt: &claimed, StartedAt: &claimed,
	}
	if err := backing.Save(ctx, r); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}

	// Wider than one batch and not a multiple of it, so a lost batch cannot hide behind a round number.
	const hosts = 1500
	summaries := make([]run.HostSummary, 0, hosts)
	tasks := make([]run.TaskSummary, 0, hosts)
	for i := range hosts {
		summaries = append(summaries, run.HostSummary{
			Host: fmt.Sprintf("web-%04d", i), OK: 1, Changed: 1, Worst: "changed", RanAt: claimed,
		})
		tasks = append(tasks, run.TaskSummary{
			Task: fmt.Sprintf("task-%04d", i), Seconds: 1, RanAt: claimed,
		})
	}

	if err := c.SaveHostSummary(ctx, r.ID, summaries); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	if err := c.SaveTaskSummary(ctx, r.ID, tasks); err != nil {
		t.Fatalf("SaveTaskSummary() error = %v", err)
	}

	gotHosts, err := backing.RunHostSummaries(ctx, r.ID)
	if err != nil {
		t.Fatalf("RunHostSummaries() error = %v", err)
	}
	if len(gotHosts) != hosts {
		t.Errorf("the control node stored %d of %d host outcomes, so the run's committed record, its "+
			"receipt, and every fleet view undercount what it touched", len(gotHosts), hosts)
	}
	// The first host is the one a batch-replacing endpoint drops first, so name it.
	seen := make(map[string]bool, len(gotHosts))
	for _, h := range gotHosts {
		seen[h.Host] = true
	}
	for _, want := range []string{"web-0000", "web-0999", "web-1000", "web-1499"} {
		if !seen[want] {
			t.Errorf("host %s is missing from the stored outcomes", want)
		}
	}

	gotTasks, err := backing.RunTaskSummaries(ctx, r.ID)
	if err != nil {
		t.Fatalf("RunTaskSummaries() error = %v", err)
	}
	if len(gotTasks) != hosts {
		t.Errorf("the control node stored %d of %d task timings", len(gotTasks), hosts)
	}
}

// TestARelayReplacesRatherThanAccumulatesAcrossReports checks the other half of the semantics. A worker
// reports its summaries more than once as a run progresses, and each report is the whole truth as it
// stands, so a later one has to replace an earlier one rather than pile on top of it. Making
// continuation batches merge must not turn a fresh report into an append.
func TestARelayReplacesRatherThanAccumulatesAcrossReports(t *testing.T) {
	t.Parallel()
	for _, backend := range storeBackends() {
		t.Run(backend.Name, func(t *testing.T) {
			t.Parallel()
			replacesAcrossReports(t, backend.Open)
		})
	}
}

// replacesAcrossReports is TestARelayReplacesRatherThanAccumulatesAcrossReports against one store.
func replacesAcrossReports(t *testing.T, open func(*testing.T) run.Store) {
	t.Helper()
	ctx := context.Background()
	c, backing := relayOver(t, open(t))

	claimed := time.Now()
	r := &run.Run{
		ID: "run_twice", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: claimed,
		ClaimedBy: "worker-a", ClaimedAt: &claimed, StartedAt: &claimed,
	}
	if err := backing.Save(ctx, r); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}

	// A first report of three hosts, then a second report of two, which is what the run now knows.
	first := []run.HostSummary{
		{Host: "web-1", OK: 1, RanAt: claimed},
		{Host: "web-2", OK: 1, RanAt: claimed},
		{Host: "web-3", OK: 1, RanAt: claimed},
	}
	if err := c.SaveHostSummary(ctx, r.ID, first); err != nil {
		t.Fatalf("SaveHostSummary(first) error = %v", err)
	}
	second := []run.HostSummary{
		{Host: "web-1", OK: 2, Changed: 1, RanAt: claimed},
		{Host: "web-2", OK: 2, RanAt: claimed},
	}
	if err := c.SaveHostSummary(ctx, r.ID, second); err != nil {
		t.Fatalf("SaveHostSummary(second) error = %v", err)
	}

	got, err := backing.RunHostSummaries(ctx, r.ID)
	if err != nil {
		t.Fatalf("RunHostSummaries() error = %v", err)
	}
	if len(got) != len(second) {
		t.Fatalf("a fresh report of %d hosts left %d stored, so reports accumulate instead of "+
			"replacing: %+v", len(second), len(got), got)
	}
	if got[0].Host != "web-1" || got[0].OK != 2 || got[0].Changed != 1 {
		t.Errorf("the stored outcome for web-1 is %+v, want the second report's", got[0])
	}
}
