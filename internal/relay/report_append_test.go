package relay_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// countingSummaryStore wraps a store to count how the relay server persists a continued report: how
// many rows it writes, whether it reads the whole stored set back mid-report, and which write path it
// takes for each batch.
type countingSummaryStore struct {
	run.Store
	appender run.SummaryAppender

	mu              sync.Mutex
	saveHostCalls   int
	appendHostCalls int
	readHostCalls   int
	hostRowsWritten int
}

// newCountingStore wraps an in-memory store, which the relay only calls into to persist summaries.
func newCountingStore() *countingSummaryStore {
	base := run.NewMemStore()
	return &countingSummaryStore{Store: base, appender: base.(run.SummaryAppender)}
}

// SaveHostSummary counts the replacing write and the rows it carries.
func (c *countingSummaryStore) SaveHostSummary(ctx context.Context, id string, s []run.HostSummary) error {
	c.mu.Lock()
	c.saveHostCalls++
	c.hostRowsWritten += len(s)
	c.mu.Unlock()
	return c.Store.SaveHostSummary(ctx, id, s)
}

// RunHostSummaries counts the whole-set read-backs.
func (c *countingSummaryStore) RunHostSummaries(ctx context.Context, id string) ([]run.HostSummary, error) {
	c.mu.Lock()
	c.readHostCalls++
	c.mu.Unlock()
	return c.Store.RunHostSummaries(ctx, id)
}

// AppendHostSummary counts the appending write and the rows it carries.
func (c *countingSummaryStore) AppendHostSummary(ctx context.Context, id string, s []run.HostSummary) error {
	c.mu.Lock()
	c.appendHostCalls++
	c.hostRowsWritten += len(s)
	c.mu.Unlock()
	return c.appender.AppendHostSummary(ctx, id, s)
}

// AppendTaskSummary forwards; the test asserts on the host path.
func (c *countingSummaryStore) AppendTaskSummary(ctx context.Context, id string, s []run.TaskSummary) error {
	return c.appender.AppendTaskSummary(ctx, id, s)
}

// TestARelayContinuationWritesOnlyItsBatch proves a report split across batches records each host once,
// so the recording cost is linear in the fleet rather than quadratic in the batch count. The fix that
// stopped a wide run undercounting did it by reading the whole stored set back and rewriting it on every
// continuation: correct, but that writes 1+2+...+k batches of rows across k batches and reads the growing
// set back each time. A continuation now upserts only its own rows and never reads the set back
// mid-report, while the first batch still replaces so a fresh report cannot accumulate.
func TestARelayContinuationWritesOnlyItsBatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newCountingStore()
	c, _ := relayOver(t, store)

	claimed := time.Now()
	r := &run.Run{
		ID: "run_wide", Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: claimed,
		ClaimedBy: "worker-a", ClaimedAt: &claimed, StartedAt: &claimed,
	}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}

	// Three batches at the thousand-item cap: 1000 + 1000 + 500.
	const hosts = 2500
	summaries := make([]run.HostSummary, 0, hosts)
	for i := range hosts {
		summaries = append(summaries, run.HostSummary{
			Host: fmt.Sprintf("web-%04d", i), OK: 1, Worst: "ok", RanAt: claimed,
		})
	}
	if err := c.SaveHostSummary(ctx, r.ID, summaries); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}

	// Snapshot what the report itself cost, before the verifying read below adds to the counts.
	reportReads := store.readHostCalls
	reportWrites := store.hostRowsWritten
	reportSaves := store.saveHostCalls
	reportAppends := store.appendHostCalls

	// Correctness first: the whole fleet still landed.
	if got, err := store.RunHostSummaries(ctx, r.ID); err != nil {
		t.Fatalf("RunHostSummaries() error = %v", err)
	} else if len(got) != hosts {
		t.Fatalf("stored %d of %d hosts", len(got), hosts)
	}

	// Linear cost: each host written exactly once, the whole set never read back mid-report.
	if reportWrites != hosts {
		t.Errorf("wrote %d summary rows for %d hosts; a continuation that rewrites the whole set writes "+
			"far more, growing with the square of the batch count", reportWrites, hosts)
	}
	if reportReads != 0 {
		t.Errorf("read the whole stored set back %d times during the report, want 0; a read per "+
			"continuation is the other half of the quadratic cost", reportReads)
	}
	if reportSaves != 1 {
		t.Errorf("SaveHostSummary (replace) called %d times, want 1 (the first batch only)", reportSaves)
	}
	if reportAppends != 2 {
		t.Errorf("AppendHostSummary called %d times, want 2 (the two continuation batches)", reportAppends)
	}
}
