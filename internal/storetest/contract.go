// Package storetest provides a shared behavior contract for run.Store implementations so that
// different backends, such as the in memory store and the SQLite store, cannot drift apart.
package storetest

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// Contract runs the full run.Store contract against a fresh store from newStore. Each subtest gets
// its own store so they do not share state.
func Contract(t *testing.T, newStore func() run.Store) {
	t.Helper()
	t.Run("save and get", func(t *testing.T) { testSaveGet(t, newStore()) })
	t.Run("provenance round trip", func(t *testing.T) { testProvenance(t, newStore()) })
	t.Run("warning round trip", func(t *testing.T) { testWarning(t, newStore()) })
	t.Run("host facts", func(t *testing.T) { testHostFacts(t, newStore()) })
	t.Run("pipeline steps round trip", func(t *testing.T) { testPipelineSteps(t, newStore()) })
	t.Run("get missing", func(t *testing.T) { testGetNotFound(t, newStore()) })
	t.Run("idempotency key dedup", func(t *testing.T) { testByIdempotencyKey(t, newStore()) })
	t.Run("save updates existing", func(t *testing.T) { testSaveUpdate(t, newStore()) })
	t.Run("list newest first", func(t *testing.T) { testList(t, newStore()) })
	t.Run("list page and status counts", func(t *testing.T) { testListPage(t, newStore()) })
	t.Run("list filter by worker and holding rule", func(t *testing.T) { testListWorkerAndHeldBy(t, newStore()) })
	t.Run("pagination at volume", func(t *testing.T) { testPaginationAtVolume(t, newStore()) })
	t.Run("log append and read", func(t *testing.T) { testLog(t, newStore()) })
	t.Run("log after cursor", func(t *testing.T) { testLogAfter(t, newStore()) })
	t.Run("log stops at the capture limit and says so", func(t *testing.T) {
		testLogCap(t, newStore())
	})
	t.Run("events append and read", func(t *testing.T) { testEvents(t, newStore()) })
	t.Run("events after cursor", func(t *testing.T) { testEventsAfter(t, newStore()) })
	t.Run("shards excluded from list", func(t *testing.T) { testShards(t, newStore()) })
	t.Run("pipeline steps ordered", func(t *testing.T) { testSteps(t, newStore()) })
	t.Run("non-terminal runs", func(t *testing.T) { testNonTerminal(t, newStore()) })
	t.Run("fleet health ranking", func(t *testing.T) { testFleetHealth(t, newStore()) })
	t.Run("run summaries round trip", func(t *testing.T) { testRunSummaries(t, newStore()) })
	t.Run("append summaries accumulate", func(t *testing.T) { testAppendSummaries(t, newStore()) })
	t.Run("unrepresentable text in a summary stores the same on every backend", func(t *testing.T) {
		testSummaryUnrepresentableText(t, newStore())
	})
	t.Run("reclaim attribution is exact", func(t *testing.T) { testReclaimAttribution(t, newStore()) })
	t.Run("drift status", func(t *testing.T) { testDriftStatus(t, newStore()) })
	t.Run("drift and fleet health agree after a purge", func(t *testing.T) {
		testDriftSurvivesPurge(t, newStore())
	})
	t.Run("host costs", func(t *testing.T) { testHostCosts(t, newStore()) })
	t.Run("flaky detection", func(t *testing.T) { testFlaky(t, newStore()) })
	t.Run("host history", func(t *testing.T) { testHostHistory(t, newStore()) })
	t.Run("sub-second host order is exact and stable", func(t *testing.T) {
		testSubSecondHostOrder(t, newStore)
	})
	t.Run("task trends", func(t *testing.T) { testTaskTrends(t, newStore()) })
	t.Run("claim leases oldest", func(t *testing.T) { testClaim(t, newStore()) })
	t.Run("claim respects queue", func(t *testing.T) { testClaimQueue(t, newStore()) })
	t.Run("heartbeat and reclaim", func(t *testing.T) { testLeaseLifecycle(t, newStore()) })
	t.Run("claim mints a per-claim secret", func(t *testing.T) { testClaimSecret(t, newStore()) })
	t.Run("run timings", func(t *testing.T) { testRunTimings(t, newStore()) })
	t.Run("reclaim resolves orphaned children", func(t *testing.T) { testReclaimOrphans(t, newStore()) })
	t.Run("transition and claim are one step", func(t *testing.T) { testTransitionStatusAndClaim(t, newStore()) })
	t.Run("reclaim settles abandoned parents", func(t *testing.T) { testReclaimAbandonedParents(t, newStore()) })
	t.Run("reclaim settles an approved parent with no coordinator", func(t *testing.T) {
		testReclaimApprovedParentWithNoCoordinator(t, newStore())
	})
	t.Run("reclaim leaves a coordinated parent alone", func(t *testing.T) {
		testReclaimLeavesACoordinatedParentAlone(t, newStore())
	})
	t.Run("cancel request", func(t *testing.T) { testRequestCancel(t, newStore()) })
	t.Run("cancel pending", func(t *testing.T) { testCancelPending(t, newStore()) })
	t.Run("save keeps cancel sticky", func(t *testing.T) { testSaveKeepsCancel(t, newStore()) })
	t.Run("claim skips cancel requested", func(t *testing.T) { testClaimSkipsCancel(t, newStore()) })
	t.Run("claim skips children of settled parents", func(t *testing.T) {
		testClaimSkipsChildrenOfSettledParents(t, newStore())
	})
	t.Run("run races serialize", func(t *testing.T) {
		testRunRacesUnderConcurrency(t, newStore())
	})
	t.Run("lease clock is one clock", func(t *testing.T) {
		testLeaseClockIsOneClock(t, newStore())
	})
	t.Run("stores agree on edges", func(t *testing.T) {
		testStoreAgreementOnEdges(t, newStore())
	})
	t.Run("transition status", func(t *testing.T) { testTransitionStatus(t, newStore()) })
	t.Run("finalize running is one write", func(t *testing.T) { testFinalizeRunning(t, newStore()) })
	t.Run("running progress is fenced", func(t *testing.T) { testApplyRunningProgress(t, newStore()) })
	t.Run("unrepresentable text is stored", func(t *testing.T) { testUnrepresentableText(t, newStore()) })
	t.Run("backends agree on edges", func(t *testing.T) { testBackendEdgeParity(t, newStore()) })
	t.Run("workers", func(t *testing.T) { testWorkers(t, newStore()) })
	t.Run("retention purge", func(t *testing.T) { testPurge(t, newStore()) })
	t.Run("summary trim bounds growth", func(t *testing.T) { testTrimSummaries(t, newStore()) })
	t.Run("terminal run fences writes", func(t *testing.T) { testTerminalFence(t, newStore()) })
}

// sampleRun returns a fully populated terminal run with deterministic times.
func sampleRun(id string) *run.Run {
	created := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	started := created.Add(time.Second)
	ended := created.Add(2 * time.Second)
	code := 0
	retryOf := "run_prior"
	return &run.Run{
		ID: id, Playbook: "play.yml", Inventory: "inventory.ini",
		Status: run.StatusSucceeded, ExitCode: &code,
		CreatedAt: created, StartedAt: &started, EndedAt: &ended,
		RetryOf:   &retryOf,
		ExtraVars: map[string]any{"version": "1.2.3"},
		Outputs:   map[string]any{"built": true, "count": float64(2)},
		Tool:      "bash", Command: "echo hi", DryRun: true,
		Tags: []string{"deploy", "config"}, SkipTags: []string{"slow"},
		Verbosity: 2, Forks: 10, DiffMode: true,
		ProposedFrom: "run_check", Intent: "echo hello on the box",
		OrgID:          "org_sample",
		IdempotencyKey: "idem_sample",
		Timeout:        3600,
		Notifications: []run.NotifyTarget{
			{Kind: run.NotifySlack, URL: "https://hooks.example.com/team", OnFailure: true},
		},
	}
}

// intPtr returns a pointer to n, for the nullable index columns above.
func intPtr(n int) *int { return &n }

// saveFinishedRun writes a run, its host summaries, then finalizes it, which is the order a real
// run takes. Summary writes are fenced once a run is terminal, so the summary has to land first.
func saveFinishedRun(t *testing.T, store run.Store, id string, at time.Time, dry bool,
	sums []run.HostSummary,
) {
	t.Helper()
	ctx := context.Background()
	r := &run.Run{ID: id, Playbook: "site.yml", Status: run.StatusRunning, CreatedAt: at, DryRun: dry}
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save(%s) error = %v", id, err)
	}
	if err := store.SaveHostSummary(ctx, id, sums); err != nil {
		t.Fatalf("SaveHostSummary(%s) error = %v", id, err)
	}
	r.Status = run.StatusSucceeded
	if err := store.Save(ctx, r); err != nil {
		t.Fatalf("Save(%s) finalize error = %v", id, err)
	}
}

// hostsInHealth returns the hosts fleet health reports, sorted, so a test can compare the set of
// hosts the two fleet views know about without depending on failure ranking.
func hostsInHealth(ctx context.Context, store run.Store) ([]string, error) {
	health, err := store.FleetHealth(ctx, 10)
	if err != nil {
		return nil, err
	}
	hosts := make([]string, 0, len(health))
	for _, h := range health {
		hosts = append(hosts, h.Host)
	}
	sort.Strings(hosts)
	return hosts, nil
}

// concatChunks joins chunk bytes in order for comparing log contents.
func concatChunks(chunks []run.LogChunk) string {
	var out []byte
	for _, c := range chunks {
		out = append(out, c.Data...)
	}
	return string(out)
}

// race runs fn n times concurrently, released together so the calls actually overlap.
func race(n int, fn func(i int)) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
}

// settledReporter is the sweep-with-attribution capability the dispatcher's janitor uses. Declared
// here so the contract can hold every backend to the same attribution rules.
type settledReporter interface {
	ReclaimStaleSettled(ctx context.Context, ttl time.Duration) (int, []string, error)
}
