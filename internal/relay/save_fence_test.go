package relay_test

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// staleSnapshotStore hands the relay handler a stale running view of one run while the underlying
// store already holds a settled row, which is exactly the interleaving where a janitor sweep lands
// between the handler's read and its write.
type staleSnapshotStore struct {
	run.Store

	mu    sync.Mutex
	stale *run.Run
	// remaining is how many Gets still answer with the stale view.
	remaining int
}

// AppendHostSummary forwards so the wrapper still satisfies the appender the handler requires.
func (s *staleSnapshotStore) AppendHostSummary(ctx context.Context, id string, sums []run.HostSummary) error {
	return s.Store.(run.SummaryAppender).AppendHostSummary(ctx, id, sums)
}

// AppendTaskSummary forwards for the same reason.
func (s *staleSnapshotStore) AppendTaskSummary(ctx context.Context, id string, sums []run.TaskSummary) error {
	return s.Store.(run.SummaryAppender).AppendTaskSummary(ctx, id, sums)
}

// Get returns the stale running view while any charges remain, then the truth.
func (s *staleSnapshotStore) Get(ctx context.Context, id string) (*run.Run, error) {
	s.mu.Lock()
	if s.remaining > 0 && s.stale != nil && s.stale.ID == id {
		s.remaining--
		cp := *s.stale
		s.mu.Unlock()
		return &cp, nil
	}
	s.mu.Unlock()
	return s.Store.Get(ctx, id)
}

// TestAWorkerReportCannotResurrectASettledRun proves the relay's terminal save is fenced. The handler
// used to Get, check, and Save: every guard ran against the snapshot, and a janitor that settled the
// run between the read and the whole-row write was silently overwritten, resurrecting the run under a
// stale lease and minting a second, contradictory outcome entry that poisons the run's receipt. The
// terminal transition now goes through the same compare-and-swap the in-process executor uses, so the
// sweep's word stands and the late report is refused.
func TestAWorkerReportCannotResurrectASettledRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	wrapped := &staleSnapshotStore{Store: backing}
	ts := httptest.NewServer(relay.NewHandler(wrapped, relay.SinglePool(testWorkerToken), nil, nil, nil))
	t.Cleanup(ts.Close)
	c := relay.NewClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, ts.Client()))

	// The run as the worker last knew it: running, claimed the older way with no per-claim secret,
	// so the report authenticates by lease name.
	claimed := time.Now()
	running := &run.Run{
		ID: "run_raced", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: claimed,
		ClaimedBy: "worker-a", ClaimedAt: &claimed, StartedAt: &claimed,
	}
	// The truth in the store: the janitor already settled it as interrupted and cleared the lease.
	settled := *running
	settled.Status = run.StatusInterrupted
	settled.ClaimedBy = ""
	settled.ClaimedAt = nil
	if err := backing.Save(ctx, &settled); err != nil {
		t.Fatalf("seed settled run: %v", err)
	}
	// The handler's own read answers with the stale running view, exactly what a Get that raced the
	// sweep saw.
	wrapped.stale = running
	wrapped.remaining = 1

	code := 0
	report := *running
	report.Status = run.StatusSucceeded
	report.ExitCode = &code
	if err := c.Save(ctx, &report); err == nil {
		t.Error("a terminal report over a settled run was accepted, want a refusal")
	}

	got, err := backing.Get(ctx, "run_raced")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != run.StatusInterrupted {
		t.Fatalf("the settled run is now %q: the worker's late report resurrected a run the "+
			"janitor had already settled", got.Status)
	}
	if got.ClaimedBy != "" {
		t.Errorf("the cleared lease came back as %q", got.ClaimedBy)
	}
}

// TestARunningReportCannotResurrectASettledRun proves the non-terminal (progress) branch of the save
// handler is fenced too. The terminal branch went through the compare-and-swap, but a running
// progress report racing the same sweep still did a plain whole-row save that put the run back to
// running under its stale lease, and a later terminal report then committed a second, contradictory
// outcome. The fence now refuses a report on a run the sweep has settled.
func TestARunningReportCannotResurrectASettledRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	wrapped := &staleSnapshotStore{Store: backing}
	ts := httptest.NewServer(relay.NewHandler(wrapped, relay.SinglePool(testWorkerToken), nil, nil, nil))
	t.Cleanup(ts.Close)
	c := relay.NewClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, ts.Client()))

	claimed := time.Now()
	running := &run.Run{
		ID: "run_raced2", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: claimed,
		ClaimedBy: "worker-a", ClaimedAt: &claimed, StartedAt: &claimed,
	}
	settled := *running
	settled.Status = run.StatusInterrupted
	settled.ClaimedBy = ""
	settled.ClaimedAt = nil
	if err := backing.Save(ctx, &settled); err != nil {
		t.Fatalf("seed settled run: %v", err)
	}
	// The handler's own read answers with the stale running view; every later read sees the truth.
	wrapped.stale = running
	wrapped.remaining = 1

	report := *running // status stays running: a progress report, not a terminal one
	if err := c.Save(ctx, &report); err == nil {
		t.Error("a running progress report over a settled run was accepted, want a refusal")
	}
	got, err := backing.Get(ctx, "run_raced2")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != run.StatusInterrupted {
		t.Fatalf("the settled run is now %q: a progress report resurrected it", got.Status)
	}
	if got.ClaimedBy != "" {
		t.Errorf("the cleared lease came back as %q", got.ClaimedBy)
	}
}
