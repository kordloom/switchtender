package sqlitestore_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// benchRuns and benchAudit are the fleet-scale shapes these benchmarks measure against: enough
// top-level runs that a list has to be paged and enough audit entries that a scan of the whole
// chain is visibly different from a bounded read.
const (
	benchRuns    = 1000
	benchEntries = 10000
)

// benchStatuses cycles the statuses a populated store holds so a status filter selects a real
// fraction of the table rather than all of it or none of it.
var benchStatuses = []run.Status{
	run.StatusSucceeded, run.StatusFailed, run.StatusRunning, run.StatusPending,
}

// benchStore opens a temp-file store, fills it with benchRuns top-level runs, their shards, host
// and task summaries, and benchEntries audit entries, and returns the bundle. A file rather than
// memory, because the split read and write pools only separate on a real path, which is the
// shape a shipped install runs in.
func benchStore(b *testing.B) *sqlitestore.DB {
	b.Helper()
	db, err := sqlitestore.Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatalf("Open() error = %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	runs := db.Runs()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range benchRuns {
		id := fmt.Sprintf("run-%05d", i)
		r := &run.Run{
			ID:        id,
			Playbook:  fmt.Sprintf("site-%d.yml", i%37),
			Inventory: "inventory/prod",
			Status:    benchStatuses[i%len(benchStatuses)],
			CreatedAt: base.Add(time.Duration(i) * time.Second),
			Tool:      "ansible",
			Queue:     "default",
			Source:    "api",
			SourceID:  fmt.Sprintf("tmpl-%d", i%11),
			Actor:     fmt.Sprintf("user-%d", i%13),
			Labels:    map[string]string{"env": "prod", "app.tier": "web"},
		}
		if err := runs.Save(ctx, r); err != nil {
			b.Fatalf("Save() error = %v", err)
		}
		// Every tenth run is a split with two shards, so parent-only listings have children to
		// exclude and the detail path has children to gather.
		if i%10 == 0 {
			for shard := range 2 {
				idx := shard
				parent := id
				child := &run.Run{
					ID:         fmt.Sprintf("%s-s%d", id, shard),
					Playbook:   r.Playbook,
					Inventory:  r.Inventory,
					Status:     r.Status,
					CreatedAt:  r.CreatedAt,
					ParentID:   &parent,
					ShardIndex: &idx,
					Tool:       "ansible",
				}
				if err := runs.Save(ctx, child); err != nil {
					b.Fatalf("Save(shard) error = %v", err)
				}
			}
		}
		hosts := make([]run.HostSummary, 4)
		tasks := make([]run.TaskSummary, 4)
		for h := range 4 {
			hosts[h] = run.HostSummary{
				RunID: id, Host: fmt.Sprintf("host-%03d", (i*4+h)%200), OK: 10, Changed: h,
				Worst: "ok", DurationSeconds: float64(h) + 1, RanAt: r.CreatedAt,
			}
			tasks[h] = run.TaskSummary{
				RunID: id, Task: fmt.Sprintf("task-%02d", h), Seconds: float64(h) + 1,
				RanAt: r.CreatedAt,
			}
		}
		if err := runs.SaveHostSummary(ctx, id, hosts); err != nil {
			b.Fatalf("SaveHostSummary() error = %v", err)
		}
		if err := runs.SaveTaskSummary(ctx, id, tasks); err != nil {
			b.Fatalf("SaveTaskSummary() error = %v", err)
		}
	}

	audits := db.Audits()
	for i := range benchEntries {
		e := &audit.Entry{
			ID:     fmt.Sprintf("aud-%06d", i),
			At:     base.Add(time.Duration(i) * time.Millisecond),
			Actor:  fmt.Sprintf("user-%d", i%13),
			Method: "POST",
			Path:   fmt.Sprintf("/api/v1/runs/run-%05d", i%benchRuns),
		}
		if err := audits.Append(ctx, e); err != nil {
			b.Fatalf("Append() error = %v", err)
		}
	}
	return db
}

// BenchmarkListPage measures the runs view's first page, the read every dashboard load makes.
func BenchmarkListPage(b *testing.B) {
	db := benchStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		if _, err := db.Runs().ListPage(ctx, run.ListFilter{}, 50, 0); err != nil {
			b.Fatalf("ListPage() error = %v", err)
		}
	}
}

// BenchmarkListPageDeepOffset measures a late page, where the offset has to be walked.
func BenchmarkListPageDeepOffset(b *testing.B) {
	db := benchStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		if _, err := db.Runs().ListPage(ctx, run.ListFilter{}, 50, 900); err != nil {
			b.Fatalf("ListPage() error = %v", err)
		}
	}
}

// BenchmarkListPageStatusFilter measures the status chip, the most-clicked filter on the runs view.
func BenchmarkListPageStatusFilter(b *testing.B) {
	db := benchStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		f := run.ListFilter{Status: string(run.StatusFailed)}
		if _, err := db.Runs().ListPage(ctx, f, 50, 0); err != nil {
			b.Fatalf("ListPage() error = %v", err)
		}
	}
}

// BenchmarkListPageHostFilter measures the host-scoped run list, which reaches into the summaries.
func BenchmarkListPageHostFilter(b *testing.B) {
	db := benchStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		if _, err := db.Runs().ListPage(ctx, run.ListFilter{Host: "host-042"}, 50, 0); err != nil {
			b.Fatalf("ListPage() error = %v", err)
		}
	}
}

// BenchmarkList measures the unbounded listing, which decodes every top-level run row.
func BenchmarkList(b *testing.B) {
	db := benchStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		if _, err := db.Runs().List(ctx); err != nil {
			b.Fatalf("List() error = %v", err)
		}
	}
}

// BenchmarkRunStatusCounts measures the status chips beside the run list.
func BenchmarkRunStatusCounts(b *testing.B) {
	db := benchStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		if _, err := db.Runs().RunStatusCounts(ctx); err != nil {
			b.Fatalf("RunStatusCounts() error = %v", err)
		}
	}
}

// BenchmarkRunDetail measures one run page: the run, its shards, and its summaries.
func BenchmarkRunDetail(b *testing.B) {
	db := benchStore(b)
	ctx := context.Background()
	runs := db.Runs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		id := fmt.Sprintf("run-%05d", (i*10)%benchRuns)
		if _, err := runs.Get(ctx, id); err != nil {
			b.Fatalf("Get() error = %v", err)
		}
		if _, err := runs.Shards(ctx, id); err != nil {
			b.Fatalf("Shards() error = %v", err)
		}
		if _, err := runs.RunHostSummaries(ctx, id); err != nil {
			b.Fatalf("RunHostSummaries() error = %v", err)
		}
		if _, err := runs.RunTaskSummaries(ctx, id); err != nil {
			b.Fatalf("RunTaskSummaries() error = %v", err)
		}
	}
}

// BenchmarkRunTimings measures the metrics scrape, which reads the newest runs narrow.
func BenchmarkRunTimings(b *testing.B) {
	db := benchStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		if _, err := db.Runs().RunTimings(ctx, benchRuns); err != nil {
			b.Fatalf("RunTimings() error = %v", err)
		}
	}
}

// BenchmarkFleetHealth measures the fleet table, a window function over every host summary.
func BenchmarkFleetHealth(b *testing.B) {
	db := benchStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		if _, err := db.Runs().FleetHealth(ctx, 10); err != nil {
			b.Fatalf("FleetHealth() error = %v", err)
		}
	}
}

// BenchmarkWorkers measures the worker listing, which groups leases out of the runs table.
func BenchmarkWorkers(b *testing.B) {
	db := benchStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		if _, err := db.Runs().Workers(ctx); err != nil {
			b.Fatalf("Workers() error = %v", err)
		}
	}
}

// BenchmarkNonTerminal measures the dispatcher's sweep over unfinished runs.
func BenchmarkNonTerminal(b *testing.B) {
	db := benchStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		if _, err := db.Runs().NonTerminal(ctx); err != nil {
			b.Fatalf("NonTerminal() error = %v", err)
		}
	}
}

// BenchmarkAuditList measures the audit view's first page.
func BenchmarkAuditList(b *testing.B) {
	db := benchStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		if _, err := db.Audits().List(ctx, 100); err != nil {
			b.Fatalf("List() error = %v", err)
		}
	}
}

// BenchmarkAuditAppend measures one chain append, which reads the head and inserts in one
// transaction. It is the per-request cost every mutating API call pays.
func BenchmarkAuditAppend(b *testing.B) {
	db := benchStore(b)
	ctx := context.Background()
	audits := db.Audits()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		e := &audit.Entry{
			ID:     fmt.Sprintf("bench-aud-%09d", i),
			At:     time.Now(),
			Actor:  "bench",
			Method: "POST",
			Path:   "/api/v1/runs",
		}
		if err := audits.Append(ctx, e); err != nil {
			b.Fatalf("Append() error = %v", err)
		}
	}
}

// BenchmarkAuditChainScan measures verifying the whole trail, which streams every entry.
func BenchmarkAuditChainScan(b *testing.B) {
	db := benchStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		n := 0
		err := db.Audits().ChainScan(ctx, 0, func(*audit.Entry) error {
			n++
			return nil
		})
		if err != nil {
			b.Fatalf("ChainScan() error = %v", err)
		}
	}
}

// BenchmarkSaveHostFacts measures storing one gather's facts for a two hundred host inventory,
// which is the shape a fleet-wide fact refresh writes.
func BenchmarkSaveHostFacts(b *testing.B) {
	db := benchStore(b)
	ctx := context.Background()
	facts := make([]run.HostFacts, 200)
	for i := range facts {
		facts[i] = run.HostFacts{
			Host:       fmt.Sprintf("host-%03d", i),
			RunID:      "run-00000",
			Facts:      map[string]string{"os": "linux", "arch": "arm64"},
			GatheredAt: time.Now(),
		}
	}
	b.ResetTimer()
	for b.Loop() {
		if err := db.Runs().SaveHostFacts(ctx, "run-00000", facts); err != nil {
			b.Fatalf("SaveHostFacts() error = %v", err)
		}
	}
}

// BenchmarkNonTerminalMature measures the dispatcher's sweep on the shape a long-lived install has:
// a large history of finished runs with only a few still moving. It is separate from
// BenchmarkNonTerminal because the shared store holds an even spread of statuses, which hides the
// thing that actually hurts here, namely that a scan costs the whole history rather than the queue.
func BenchmarkNonTerminalMature(b *testing.B) {
	db, err := sqlitestore.Open(filepath.Join(b.TempDir(), "mature.db"))
	if err != nil {
		b.Fatalf("Open() error = %v", err)
	}
	b.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range benchRuns * 5 {
		status := run.StatusSucceeded
		if i%500 == 0 {
			status = run.StatusRunning
		}
		r := &run.Run{
			ID: fmt.Sprintf("run-%06d", i), Playbook: "site.yml", Inventory: "inventory/prod",
			Status: status, CreatedAt: base.Add(time.Duration(i) * time.Second), Tool: "ansible",
		}
		if err := db.Runs().Save(ctx, r); err != nil {
			b.Fatalf("Save() error = %v", err)
		}
	}
	b.ResetTimer()
	for b.Loop() {
		if _, err := db.Runs().NonTerminal(ctx); err != nil {
			b.Fatalf("NonTerminal() error = %v", err)
		}
	}
}
